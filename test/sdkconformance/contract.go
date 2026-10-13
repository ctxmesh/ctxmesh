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

// Package sdkconformance reads sdk/launcher-routes.json, the one definition of the launcher's
// agent-facing wire contract, and provides what checks it: a schema validator, the stub world the
// goldens are true in, and the self-consistency rules a contract must meet before anything else
// trusts it. cmd/launcher proves the goldens against the real handlers, hack/gen-sdk-fakes turns
// them into each SDK's test fake, and hack/sdk-conformance drives every SDK against a real launcher.
package sdkconformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File is the contract's path relative to the repository root.
const File = "sdk/launcher-routes.json"

// Languages are the SDKs the suite covers, by their directory under sdk/.
var Languages = []string{"go", "python", "typescript", "java", "rust", "ruby"}

// Contract is the parsed routes file.
type Contract struct {
	Description string  `json:"description,omitempty"`
	Routes      []Route `json:"routes"`
	World       World   `json:"world"`
}

// Route is one launcher route: its identity (method, path, listener, port), what it accepts, what it
// answers, and the golden examples that are true of the real handler.
type Route struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Listener    string              `json:"listener"`
	PortEnv     string              `json:"portEnv"`
	DefaultPort int                 `json:"defaultPort"`
	Summary     string              `json:"summary,omitempty"`
	Request     Request             `json:"request"`
	Responses   map[string]Response `json:"responses"`
	Goldens     []Golden            `json:"goldens"`
	// Tier is the SDK tier that owes the route (ADR 0139): TierAuthoring routes are owed only by
	// authoring SDKs. Empty means every SDK owes it.
	Tier string `json:"tier,omitempty"`
	// SDKResult lists the response fields every SDK's result must carry with the launcher's value:
	// "[]" descends into each array element and "$" is the whole body. A probe that compared only
	// what an SDK decoded would pass an SDK that silently dropped a field.
	SDKResult []string `json:"sdkResult,omitempty"`
	// SDKWaivers names, per language, why that SDK has no call reaching this route and which card
	// removes the gap. A waiver is reported as WAIVED, never PASS.
	SDKWaivers map[string]string `json:"sdkWaivers,omitempty"`
}

// Request describes what a route accepts.
type Request struct {
	// Method is the one method an ANY route actually serves; empty for method-specific routes.
	Method  string                     `json:"method,omitempty"`
	Params  map[string]json.RawMessage `json:"params,omitempty"`
	Query   map[string]json.RawMessage `json:"query,omitempty"`
	Headers map[string]Header          `json:"headers,omitempty"`
	// Body is a JSON Schema for the request body; nil means the route takes no body.
	Body json.RawMessage `json:"body,omitempty"`
	// OptionalBody allows the body to be absent altogether.
	OptionalBody bool `json:"optionalBody,omitempty"`
}

// Header describes one request header.
type Header struct {
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// Response describes one status a route answers with.
type Response struct {
	Description string `json:"description,omitempty"`
	// ContentType is the media type of the body; empty means no body.
	ContentType string          `json:"contentType,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// Golden is one request and the exact response the real launcher gives it in the World.
type Golden struct {
	Name string `json:"name"`
	// ViolatesContract marks an example whose request breaks the route's schema on purpose, to show
	// how the launcher refuses it. Every other golden's request must satisfy the schema.
	ViolatesContract bool           `json:"violatesContract,omitempty"`
	Request          GoldenRequest  `json:"request"`
	Response         GoldenResponse `json:"response"`
}

// GoldenRequest is a concrete request. Body is JSON; BodyText is a raw body that is not JSON.
type GoldenRequest struct {
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	BodyText *string           `json:"bodyText,omitempty"`
}

// GoldenResponse is the response the launcher must give. Headers is the subset asserted.
type GoldenResponse struct {
	Status int `json:"status"`
	// Stream marks a response that stays open; BodyText arrives after the World's tools change.
	Stream   bool              `json:"stream,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	BodyText *string           `json:"bodyText,omitempty"`
}

// RawBody is the golden request's body bytes, JSON or text.
func (g GoldenRequest) RawBody() []byte {
	if g.BodyText != nil {
		return []byte(*g.BodyText)
	}
	return g.Body
}

// AnyMethod is the method of a route registered without one, which any method reaches.
const AnyMethod = "ANY"

// SDK conformance tiers (ADR 0139), as each SDK declares in sdk/<lang>/.sdk-tier.
const (
	TierPlaneClient = "plane-client"
	TierAuthoring   = "authoring"
)

// OwedBy reports whether an SDK of the given tier must reach the route.
func (r Route) OwedBy(tier string) bool {
	return r.Tier == "" || r.Tier == TierPlaneClient || r.Tier == tier
}

// Exemption says why an SDK does not owe a route, or "" when it does: a waiver, which records a gap
// with the card that closes it, or the route's tier.
func (r Route) Exemption(lang, tier string) (reason string, waiver bool) {
	if why, ok := r.SDKWaivers[lang]; ok {
		return why, true
	}
	if !r.OwedBy(tier) {
		return fmt.Sprintf("an %s-tier route (ADR 0139); %s is a %s SDK", r.Tier, lang, tier), false
	}
	return "", false
}

// SDKTier reads the tier an SDK declares in sdk/<lang>/.sdk-tier.
func SDKTier(root, lang string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "sdk", lang, ".sdk-tier"))
	if err != nil {
		return "", err
	}
	tier := strings.TrimSpace(string(raw))
	if tier != TierPlaneClient && tier != TierAuthoring {
		return "", fmt.Errorf("sdk/%s/.sdk-tier declares %q, which is not a tier", lang, tier)
	}
	return tier, nil
}

// Key names a route the way every report does: "POST /delegate".
func (r Route) Key() string { return r.Method + " " + r.Path }

// ServedMethod is the method the handler accepts: Request.Method for an ANY route, else Method.
func (r Route) ServedMethod() string {
	if r.Method == AnyMethod && r.Request.Method != "" {
		return r.Request.Method
	}
	return r.Method
}

// MatchesPath reports whether a concrete path fits the route pattern; {param} is exactly one segment,
// so /memory/{conversationId} never matches /memory/agent/remember.
func (r Route) MatchesPath(path string) bool {
	want := strings.Split(strings.Trim(r.Path, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if strings.HasPrefix(want[i], "{") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// Matches reports whether method and path reach this route.
func (r Route) Matches(method, path string) bool {
	return (r.Method == AnyMethod || r.Method == method) && r.MatchesPath(path)
}

// Method returns the golden's method, defaulting to the route's served method.
func (g Golden) Method(r Route) string {
	if g.Request.Method != "" {
		return g.Request.Method
	}
	return r.ServedMethod()
}

// Load reads and parses the contract at path.
func Load(path string) (*Contract, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Contract
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Routes) == 0 {
		return nil, fmt.Errorf("%s declares no routes", path)
	}
	return &c, nil
}

// RepoRoot walks up from dir to the directory holding the contract file.
func RepoRoot(dir string) (string, error) {
	start := dir
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, File)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no %s above %s", File, start)
}

// LoadFromRepo finds the repository root above dir and loads the contract there.
func LoadFromRepo(dir string) (*Contract, string, error) {
	root, err := RepoRoot(dir)
	if err != nil {
		return nil, "", err
	}
	c, err := Load(filepath.Join(root, File))
	return c, root, err
}

// Route returns the route a method and concrete path reach, or nil.
func (c *Contract) Route(method, path string) *Route {
	for i := range c.Routes {
		if c.Routes[i].Matches(method, path) {
			return &c.Routes[i]
		}
	}
	return nil
}

// RouteByKey returns the route with the given key, or nil.
func (c *Contract) RouteByKey(key string) *Route {
	for i := range c.Routes {
		if c.Routes[i].Key() == key {
			return &c.Routes[i]
		}
	}
	return nil
}

// PathKnown reports whether any route serves path under some method, which is how a 405 differs
// from a 404 on Go's mux.
func (c *Contract) PathKnown(path string) bool {
	for i := range c.Routes {
		if c.Routes[i].MatchesPath(path) {
			return true
		}
	}
	return false
}

// Resolve reports whether path, in sdkResult notation, names a value in a decoded JSON document.
func Resolve(v any, path string) bool {
	if path == "$" {
		return true
	}
	return resolve(v, strings.Split(path, "."))
}

func resolve(v any, segs []string) bool {
	if len(segs) == 0 {
		return true
	}
	key, each := strings.CutSuffix(segs[0], "[]")
	if key != "" {
		obj, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = obj[key]; !ok {
			return false
		}
	}
	if !each {
		return resolve(v, segs[1:])
	}
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, e := range arr {
		if !resolve(e, segs[1:]) {
			return false
		}
	}
	return true
}
