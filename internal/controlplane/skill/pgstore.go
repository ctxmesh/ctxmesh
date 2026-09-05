/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// pgStore is the Postgres-backed Store. The schema is applied by the control-plane migrations
// (0026_skills.sql), not here — the store assumes the tables exist.
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a Store over the given handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) UpsertSkill(ctx context.Context, sk Skill) error {
	if err := ValidateSkill(sk); err != nil {
		return err
	}
	labels, err := json.Marshal(orEmpty(sk.Labels))
	if err != nil {
		return fmt.Errorf("skill: marshal labels: %w", err)
	}
	// Only metadata is touched. The version history is append-only and lives in another table,
	// so editing a description can never rewrite it.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO skills (namespace, name, description, labels)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (namespace, name) DO UPDATE
		  SET description = EXCLUDED.description,
		      labels      = EXCLUDED.labels,
		      updated_at  = now()`,
		sk.Namespace, sk.Name, sk.Description, labels)
	if err != nil {
		return fmt.Errorf("skill: upsert %s/%s: %w", sk.Namespace, sk.Name, err)
	}
	return nil
}

func (s *pgStore) GetSkill(ctx context.Context, ns, name string) (Skill, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT namespace, name, description, labels, created_at, updated_at
		FROM skills WHERE namespace = $1 AND name = $2`, ns, name)
	sk, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, err
	}
	return sk, true, nil
}

func (s *pgStore) ListSkills(ctx context.Context, ns string) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT namespace, name, description, labels, created_at, updated_at
		FROM skills WHERE namespace = $1 ORDER BY name ASC`, ns)
	if err != nil {
		return nil, fmt.Errorf("skill: list %s: %w", ns, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Skill{}
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *pgStore) DeleteSkill(ctx context.Context, ns, name string) error {
	// Versions and aliases cascade (the FKs in 0026_skills.sql): an orphaned version would
	// resolve for a skill nobody can see.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM skills WHERE namespace = $1 AND name = $2`, ns, name); err != nil {
		return fmt.Errorf("skill: delete %s/%s: %w", ns, name, err)
	}
	return nil
}

func (s *pgStore) AddVersion(ctx context.Context, v SkillVersion) error {
	if err := ValidateVersion(v); err != nil {
		return err
	}
	// ON CONFLICT DO NOTHING is the idempotency contract, not a shrug: the same bytes are the
	// same version, so a retry must succeed and two callers uploading identical content must
	// not fork one thing into two.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO skill_versions
		  (namespace, skill, digest, source, repo, git_ref, path, object_key, size_bytes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (namespace, skill, digest) DO NOTHING`,
		v.Namespace, v.Skill, v.Digest, string(v.Source),
		v.Repo, v.Ref, v.Path, v.ObjectKey, v.SizeBytes, v.CreatedBy)
	if err != nil {
		// A foreign-key violation means the skill does not exist. Say that, rather than
		// surfacing a constraint name the caller cannot act on.
		if strings.Contains(err.Error(), "skill_versions_namespace_skill_fkey") {
			return fmt.Errorf("skill %s/%s does not exist", v.Namespace, v.Skill)
		}
		return fmt.Errorf("skill: add version %s: %w", v.Digest, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Already present — the no-op path, which is success.
		return nil
	}
	return nil
}

func (s *pgStore) GetVersion(ctx context.Context, ns, skill, digest string) (SkillVersion, bool, error) {
	row := s.db.QueryRowContext(ctx, versionSelect+`
		WHERE namespace = $1 AND skill = $2 AND digest = $3`, ns, skill, digest)
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillVersion{}, false, nil
	}
	if err != nil {
		return SkillVersion{}, false, err
	}
	return v, true, nil
}

func (s *pgStore) ListVersions(ctx context.Context, ns, skill string) ([]SkillVersion, error) {
	// Newest first, with digest as the tiebreak so the order is TOTAL. Two versions written in
	// the same transaction share created_at, and without the tiebreak "the newest version"
	// would vary between calls — which would make `latest` resolve differently on identical data.
	rows, err := s.db.QueryContext(ctx, versionSelect+`
		WHERE namespace = $1 AND skill = $2
		ORDER BY created_at DESC, digest DESC`, ns, skill)
	if err != nil {
		return nil, fmt.Errorf("skill: list versions %s/%s: %w", ns, skill, err)
	}
	defer func() { _ = rows.Close() }()

	out := []SkillVersion{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *pgStore) ResolveAlias(ctx context.Context, ns, skill, alias string) (string, bool, error) {
	if strings.EqualFold(alias, "latest") {
		vs, err := s.ListVersions(ctx, ns, skill)
		if err != nil || len(vs) == 0 {
			return "", false, err
		}
		return vs[0].Digest, true, nil
	}
	var digest string
	err := s.db.QueryRowContext(ctx,
		`SELECT digest FROM skill_aliases WHERE namespace = $1 AND skill = $2 AND alias = $3`,
		ns, skill, alias).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("skill: resolve alias %q: %w", alias, err)
	}
	return digest, true, nil
}

func (s *pgStore) SetAlias(ctx context.Context, ns, skill, alias, digest string) error {
	// `latest` is DERIVED from the history. Allowing it to be pinned would give one name two
	// meanings — the pinned digest and the newest one — which is the ambiguity that
	// resolve-at-deploy-time exists to remove.
	if strings.EqualFold(alias, "latest") {
		return fmt.Errorf("alias %q is derived from the version history and cannot be set", alias)
	}
	// The FK enforces that the digest exists; a dangling alias would resolve to nothing at
	// deploy time, which is the worst moment to discover it.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO skill_aliases (namespace, skill, alias, digest)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (namespace, skill, alias) DO UPDATE
		  SET digest = EXCLUDED.digest, updated_at = now()`,
		ns, skill, alias, digest)
	if err != nil {
		return fmt.Errorf("cannot point %q at digest %s: %w", alias, digest, err)
	}
	return nil
}

const versionSelect = `
	SELECT namespace, skill, digest, source, repo, git_ref, path, object_key, size_bytes, created_at, created_by
	FROM skill_versions`

type scanner interface{ Scan(...any) error }

func scanSkill(row scanner) (Skill, error) {
	var (
		sk        Skill
		labelsRaw []byte
	)
	if err := row.Scan(&sk.Namespace, &sk.Name, &sk.Description, &labelsRaw,
		&sk.CreatedAt, &sk.UpdatedAt); err != nil {
		return Skill{}, err
	}
	if len(labelsRaw) > 0 {
		if err := json.Unmarshal(labelsRaw, &sk.Labels); err != nil {
			return Skill{}, fmt.Errorf("skill: unmarshal labels: %w", err)
		}
	}
	return sk, nil
}

func scanVersion(row scanner) (SkillVersion, error) {
	var (
		v   SkillVersion
		src string
	)
	if err := row.Scan(&v.Namespace, &v.Skill, &v.Digest, &src, &v.Repo, &v.Ref, &v.Path,
		&v.ObjectKey, &v.SizeBytes, &v.CreatedAt, &v.CreatedBy); err != nil {
		return SkillVersion{}, err
	}
	v.Source = SourceType(src)
	return v, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
