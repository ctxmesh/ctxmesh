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
	"fmt"
	"strings"
)

// Ref is one entry of AgentDeployment.spec.skillRefs, parsed.
type Ref struct {
	Name string
	// Version is a digest ("sha256:…") or an alias ("stable", "latest"). Which one it is
	// decides whether resolution is a lookup or a no-op, and only the digest form is safe to
	// carry into a running agent.
	Version string
}

// IsDigest reports whether the ref already names immutable content.
func (r Ref) IsDigest() bool { return strings.HasPrefix(r.Version, "sha256:") }

// String renders the canonical "<name>@<version>" form.
func (r Ref) String() string { return r.Name + "@" + r.Version }

// ParseRef parses "<name>@<version>".
//
// The version is REQUIRED. A bare name would have to mean "latest", and an implicit floating
// reference is exactly what this design refuses: it would let a skill change underneath a
// running agent while the spec that produced it looked unchanged. Making the user write
// "@latest" keeps the choice visible in the spec and in review.
func ParseRef(s string) (Ref, error) {
	name, version, found := strings.Cut(strings.TrimSpace(s), "@")
	if !found {
		return Ref{}, fmt.Errorf(
			"skill ref %q must be \"<name>@<version>\": name a digest or an alias explicitly, "+
				"so a floating reference is never implicit", s)
	}
	name, version = strings.TrimSpace(name), strings.TrimSpace(version)
	if name == "" || version == "" {
		return Ref{}, fmt.Errorf("skill ref %q must name both a skill and a version", s)
	}
	if !dns1123.MatchString(name) || len(name) > MaxNameLength {
		return Ref{}, fmt.Errorf("skill ref %q: %q is not a valid skill name", s, name)
	}
	return Ref{Name: name, Version: version}, nil
}

// Resolver turns refs into pinned refs. Separate from Store so the controller can depend on the
// narrow thing it needs.
type Resolver interface {
	// Resolve returns the ref with its Version replaced by the digest it names. A ref that is
	// already a digest resolves to itself without touching the store.
	Resolve(ctx context.Context, namespace string, r Ref) (Ref, error)

	// Describe returns name → description for the named skills. The controller injects these
	// so the launcher can answer "what skills do I have?" with no I/O — that call happens on
	// every run, and a network round-trip there would make progressive disclosure cost more
	// than it saves.
	Describe(ctx context.Context, namespace string, names []string) (map[string]string, error)
}

type storeResolver struct{ s Store }

// NewResolver returns a Resolver backed by a Store.
func NewResolver(s Store) Resolver { return storeResolver{s: s} }

func (sr storeResolver) Resolve(ctx context.Context, namespace string, r Ref) (Ref, error) {
	if r.IsDigest() {
		// Verify it EXISTS. A digest that names nothing would otherwise be recorded into the
		// AgentVersion snapshot as though it were content, and the failure would surface when
		// the agent starts rather than when the spec was applied.
		if _, ok, err := sr.s.GetVersion(ctx, namespace, r.Name, r.Version); err != nil {
			return Ref{}, err
		} else if !ok {
			return Ref{}, fmt.Errorf("skill %q has no version %s", r.Name, r.Version)
		}
		return r, nil
	}
	digest, ok, err := sr.s.ResolveAlias(ctx, namespace, r.Name, r.Version)
	if err != nil {
		return Ref{}, err
	}
	if !ok {
		return Ref{}, fmt.Errorf("skill %q has no version or alias %q", r.Name, r.Version)
	}
	return Ref{Name: r.Name, Version: digest}, nil
}

// ResolveAll parses and resolves every entry, returning the PINNED refs in input order.
//
// All-or-nothing on purpose: a partially resolved list would deploy an agent with some skills
// missing and no signal that anything was dropped, which is worse than refusing the spec.
func ResolveAll(ctx context.Context, res Resolver, namespace string, refs []string) ([]string, error) {
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, raw := range refs {
		r, err := ParseRef(raw)
		if err != nil {
			return nil, err
		}
		// The same skill twice would inject its description twice and double its always-on
		// context cost, for nothing.
		if seen[r.Name] {
			return nil, fmt.Errorf("skill %q is attached more than once", r.Name)
		}
		seen[r.Name] = true

		pinned, err := res.Resolve(ctx, namespace, r)
		if err != nil {
			return nil, err
		}
		out = append(out, pinned.String())
	}
	return out, nil
}

func (sr storeResolver) Describe(ctx context.Context, namespace string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, n := range names {
		sk, ok, err := sr.s.GetSkill(ctx, namespace, n)
		if err != nil {
			return nil, err
		}
		// A skill with no description is legal — it is simply harder for a model to know when
		// it applies. Omitting the key rather than storing "" keeps the injected JSON small.
		if ok && sk.Description != "" {
			out[n] = sk.Description
		}
	}
	return out, nil
}
