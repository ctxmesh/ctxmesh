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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds. A skill's description is ALWAYS-ON context cost — progressive disclosure exists
// precisely because context is scarce — so it is capped for the same reason
// CapabilityDescriptor bounds tags at 16. The body cap is deliberately generous: a procedure
// truncated halfway is worse than one that was never loaded.
const (
	MaxDescriptionBytes = 1024
	MaxNameLength       = 63
	MaxBodyBytes        = 1 << 20 // 1 MiB
)

// Bounds on a version's pointers. They are echoed into the AgentDeployment's SkillBodies condition,
// whose message the API server caps, so an unbounded one could keep every agent attaching the skill
// from writing its status.
const (
	MaxRepoLength      = 512
	MaxRefLength       = 255
	MaxPathLength      = 1024
	MaxObjectKeyLength = 1024
)

// canonicalDigest is the only digest form a version can have: Digest's output.
var canonicalDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// keySegment is what one segment of an uploaded version's object key may hold.
var keySegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// dns1123 is the name shape every Kubernetes-adjacent identifier in this product uses.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// fullSHA matches a 40- or 64-hex git object id.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// ValidateSkill checks a skill's metadata. The CRD markers that would have done this in the API
// server do not exist for a Postgres-resident entity (ADR 0044 §2), so validation is a Go
// function called before every write, returning a typed error the handler maps to 422.
func ValidateSkill(s Skill) error {
	if !dns1123.MatchString(s.Name) || len(s.Name) > MaxNameLength {
		return fmt.Errorf("skill name %q must be a DNS-1123 label of at most %d characters", s.Name, MaxNameLength)
	}
	if strings.TrimSpace(s.Namespace) == "" {
		return fmt.Errorf("skill %q has no namespace", s.Name)
	}
	if len(s.Description) > MaxDescriptionBytes {
		return fmt.Errorf(
			"skill %q description is %d bytes, over the %d-byte cap: it sits in every agent's context permanently",
			s.Name, len(s.Description), MaxDescriptionBytes)
	}
	return nil
}

// ValidateVersion checks a version is resolvable and reproducible.
//
// The git rule is the load-bearing one. A branch name is refused because a version that can
// change underneath a deployment is not a version — the same reasoning GitPromptSource states
// for prompts, and it matters more here: record/replay fixtures and the eval gate both assume a
// pinned artifact, so a skill that moved would make a green fixture a lie.
func ValidateVersion(v SkillVersion) error {
	if strings.TrimSpace(v.Skill) == "" || strings.TrimSpace(v.Namespace) == "" {
		return fmt.Errorf("skill version must name a namespace and a skill")
	}
	if v.Digest == "" {
		return fmt.Errorf("skill version has no digest — the digest IS its identity")
	}
	if !ValidDigest(v.Digest) {
		return fmt.Errorf("skill version digest %q must be \"sha256:\" and 64 lowercase hex characters, "+
			"the sha256 of the body's bytes", truncate(v.Digest, 80))
	}
	switch v.Source {
	case SourceGit:
		if v.Repo == "" || v.Ref == "" || v.Path == "" {
			return fmt.Errorf("git-sourced skill version needs repo, ref and path")
		}
		if v.ObjectKey != "" {
			return fmt.Errorf("git-sourced skill version must not also carry an object key")
		}
		if err := ValidGitPin(v.Repo, v.Ref, v.Path); err != nil {
			return err
		}
	case SourceUpload:
		if v.ObjectKey == "" {
			return fmt.Errorf("uploaded skill version has no object key — the bytes live in the object store, not here")
		}
		if !OwnsObjectKey(v.Namespace, v.ObjectKey) {
			return fmt.Errorf("uploaded skill version object key %q must be under %q, at most %d characters, "+
				"in clean form, with each segment made of letters, digits, '.', '_' and '-': the controller "+
				"reads uploaded bodies with the platform's store credential, so a key outside the namespace's "+
				"prefix would read another namespace's objects",
				truncate(v.ObjectKey, 200), ObjectKeyPrefix(v.Namespace), MaxObjectKeyLength)
		}
		if v.Repo != "" || v.Ref != "" || v.Path != "" {
			return fmt.Errorf("uploaded skill version must not also carry a git pin")
		}
	default:
		return fmt.Errorf("skill version source %q must be %q or %q", v.Source, SourceGit, SourceUpload)
	}
	if v.SizeBytes > MaxBodyBytes {
		return fmt.Errorf("skill version is %d bytes, over the %d-byte cap", v.SizeBytes, MaxBodyBytes)
	}
	return nil
}

// ObjectKeyPrefix is the object-store prefix a namespace's uploaded skill bodies live under. The
// store is shared by every namespace and the controller reads it with one credential, so this prefix
// is the only thing keeping one namespace's skill from naming another namespace's object.
func ObjectKeyPrefix(namespace string) string {
	return "skills/" + namespace + "/"
}

// OwnsObjectKey reports whether key names an object under the namespace's prefix. The key must be in
// clean form, because "skills/a/../b/x" has the prefix as text but names an object of namespace b, and
// each segment is restricted to letters, digits, '.', '_' and '-', so no store reads one differently.
func OwnsObjectKey(namespace, key string) bool {
	prefix := ObjectKeyPrefix(namespace)
	if namespace == "" || len(key) > MaxObjectKeyLength || !strings.HasPrefix(key, prefix) ||
		len(key) == len(prefix) || path.Clean(key) != key {
		return false
	}
	for seg := range strings.SplitSeq(key, "/") {
		if !keySegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// ValidDigest reports whether d is a version digest in canonical form. Any other spelling, such as an
// upper-case one, can never equal a body's Digest, so a version carrying it could never be mounted.
func ValidDigest(d string) bool {
	return canonicalDigest.MatchString(d)
}

// ValidGitPin checks a git pointer is bounded, pinned and in clean form. The ref and path become part
// of the URL the controller fetches, so a ".." segment, a query or a fragment in either could make it
// read a file the pointer does not name.
func ValidGitPin(repo, ref, filePath string) error {
	switch {
	case len(repo) > MaxRepoLength:
		return fmt.Errorf("skill version repo is %d characters, over the %d-character cap", len(repo), MaxRepoLength)
	case len(ref) > MaxRefLength:
		return fmt.Errorf("skill version ref is %d characters, over the %d-character cap", len(ref), MaxRefLength)
	case len(filePath) > MaxPathLength:
		return fmt.Errorf("skill version path is %d characters, over the %d-character cap", len(filePath), MaxPathLength)
	}
	if !isImmutableRef(ref) {
		return fmt.Errorf(
			"skill version ref %q is not an immutable pin: use a full commit SHA or a tag, "+
				"never a branch — a version that can change underneath a deployment is not a version",
			truncate(ref, 80))
	}
	for _, p := range []struct{ name, value string }{{"ref", ref}, {"path", filePath}} {
		if strings.HasPrefix(p.value, "/") || path.Clean(p.value) != p.value ||
			strings.ContainsAny(p.value, "?#%\\") || strings.ContainsFunc(p.value, unicode.IsControl) {
			return fmt.Errorf("skill version %s %q must be a clean relative path with no '.' or '..' "+
				"segment and no '?', '#', '%%' or '\\'", p.name, truncate(p.value, 80))
		}
	}
	return nil
}

// truncate shortens s for an error message, at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// IsImmutableRef reports whether a git ref is a pin a version may name: a full commit SHA, or a tag.
func IsImmutableRef(ref string) bool { return isImmutableRef(ref) }

// isImmutableRef reports whether a git ref pins content that cannot move.
//
// A full SHA always does. Anything else is accepted only if it is NOT a plain branch-looking
// name: refs/tags/... is explicit and pinned. Bare names like "main" or "release" are refused —
// they read like branches, and guessing wrong here silently costs reproducibility rather than
// failing loudly, which is the worse of the two errors.
func isImmutableRef(ref string) bool {
	if fullSHA.MatchString(ref) {
		return true
	}
	return strings.HasPrefix(ref, "refs/tags/")
}

// Digest is the content hash that identifies a version. Exported so the upload path and the git
// resolver produce identical identities for identical bytes — two skills with the same content
// ARE the same version, whichever door they came through, and that property is what makes
// re-adding a digest an idempotent no-op instead of a fork.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
