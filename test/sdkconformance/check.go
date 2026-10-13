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

package sdkconformance

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, AnyMethod: true}

// Check reports every way the contract disagrees with itself. Nothing downstream may trust a contract
// that fails it: a golden that violates its own route's schema would teach every fake a wrong shape.
func (c *Contract) Check() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range c.Routes {
		if seen[r.Key()] {
			out = append(out, r.Key()+": declared twice")
		}
		seen[r.Key()] = true
		out = append(out, prefixed(r.Key(), r.check())...)
	}
	return append(out, c.World.check()...)
}

func prefixed(p string, msgs []string) []string {
	for i := range msgs {
		msgs[i] = p + ": " + msgs[i]
	}
	return msgs
}

func (r Route) check() []string {
	var out []string
	switch {
	case !methods[r.Method]:
		out = append(out, "unknown method "+r.Method)
	case r.Method == AnyMethod && r.Request.Method == "":
		out = append(out, "an ANY route must name the method it serves in request.method")
	}
	if r.Listener == "" || r.DefaultPort == 0 {
		out = append(out, "listener and defaultPort are required (portEnv is empty only for a fixed port)")
	}
	if r.Tier != "" && r.Tier != TierPlaneClient && r.Tier != TierAuthoring {
		out = append(out, fmt.Sprintf("tier %q is not a tier (%s or %s)", r.Tier, TierPlaneClient, TierAuthoring))
	}
	out = append(out, r.checkSchemas()...)
	for lang, why := range r.SDKWaivers {
		if !slices.Contains(Languages, lang) {
			out = append(out, fmt.Sprintf("waiver for unknown language %q", lang))
		}
		if strings.TrimSpace(why) == "" {
			out = append(out, "the "+lang+" waiver gives no reason")
		}
	}
	if len(r.Goldens) == 0 {
		return append(out, "has no golden example; every route needs at least one")
	}
	if len(r.SDKResult) > 0 {
		body, err := Decode(r.Goldens[0].Response.Body)
		for _, path := range r.SDKResult {
			if err != nil || !Resolve(body, path) {
				out = append(out, "sdkResult "+path+" names nothing in the first golden's response, so requiring it proves nothing")
			}
		}
	}
	if s := r.Goldens[0].Response.Status; s < 200 || s > 299 {
		out = append(out, fmt.Sprintf("the first golden answers %d; it must be the success example SDK fakes serve", s))
	}
	names := map[string]bool{}
	for _, g := range r.Goldens {
		if g.Name == "" || names[g.Name] {
			out = append(out, fmt.Sprintf("golden name %q is empty or repeated", g.Name))
		}
		names[g.Name] = true
		out = append(out, prefixed("golden "+g.Name, r.checkGolden(g))...)
	}
	return out
}

func (r Route) checkSchemas() []string {
	var out []string
	if r.Request.Body != nil {
		out = append(out, prefixed("request.body", CheckSchema(r.Request.Body))...)
	}
	for _, k := range slices.Sorted(maps.Keys(r.Request.Params)) {
		out = append(out, prefixed("request.params."+k, CheckSchema(r.Request.Params[k]))...)
	}
	for _, k := range slices.Sorted(maps.Keys(r.Request.Query)) {
		out = append(out, prefixed("request.query."+k, CheckSchema(r.Request.Query[k]))...)
	}
	if len(r.Responses) == 0 {
		out = append(out, "declares no responses")
	}
	for _, code := range slices.Sorted(maps.Keys(r.Responses)) {
		n, err := strconv.Atoi(code)
		if err != nil || http.StatusText(n) == "" {
			out = append(out, "response key "+code+" is not an HTTP status")
		}
		resp := r.Responses[code]
		if resp.Schema != nil {
			if !IsJSON(resp.ContentType) {
				out = append(out, "response "+code+" has a schema but a non-JSON content type")
			}
			out = append(out, prefixed("response "+code, CheckSchema(resp.Schema))...)
		}
	}
	return out
}

func (r Route) checkGolden(g Golden) []string {
	var out []string
	method := g.Method(r)
	if r.Method != AnyMethod && method != r.Method {
		out = append(out, "method "+method+" does not reach this route")
	}
	if !r.MatchesPath(g.Request.Path) {
		out = append(out, "path "+g.Request.Path+" does not match "+r.Path)
	}
	if g.Request.Body != nil && g.Request.BodyText != nil {
		out = append(out, "request has both body and bodyText")
	}
	decl, ok := r.Responses[strconv.Itoa(g.Response.Status)]
	if !ok {
		return append(out, fmt.Sprintf("answers %d, which the route does not declare", g.Response.Status))
	}
	out = append(out, checkGoldenResponse(decl, g.Response)...)
	violations := r.RequestViolations(g.Request.Path, g.Request.Query, g.Request.Headers, g.Request.RawBody())
	if method != r.ServedMethod() {
		violations = append(violations, "the route serves "+r.ServedMethod()+", not "+method)
	}
	switch {
	case g.ViolatesContract && len(violations) == 0:
		out = append(out, "is marked violatesContract but its request satisfies the contract")
	case !g.ViolatesContract:
		out = append(out, prefixed("request", violations)...)
	}
	return out
}

func checkGoldenResponse(decl Response, resp GoldenResponse) []string {
	var out []string
	switch {
	case resp.Body != nil && resp.BodyText != nil:
		out = append(out, "response has both body and bodyText")
	case resp.Body != nil:
		if !IsJSON(decl.ContentType) {
			out = append(out, "response has a JSON body but the status declares "+decl.ContentType)
		}
		v, err := Decode(resp.Body)
		if err != nil {
			out = append(out, "response body: "+err.Error())
		} else if decl.Schema != nil {
			out = append(out, prefixed("response body", Validate(decl.Schema, v))...)
		}
	case resp.BodyText != nil:
		if decl.ContentType == "" || IsJSON(decl.ContentType) {
			out = append(out, "response has a text body but the status declares "+decl.ContentType)
		}
	}
	if resp.Stream && resp.BodyText == nil {
		out = append(out, "a stream golden needs bodyText: the transcript it expects")
	}
	return out
}

// RequestViolations reports how a request breaks the route's contract: a missing required header, a
// path or query parameter outside its schema, or a body outside the request schema.
func (r Route) RequestViolations(path string, query, headers map[string]string, body []byte) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(query)) {
		s, ok := r.Request.Query[k]
		if !ok {
			out = append(out, "query parameter "+k+" is not in the contract")
			continue
		}
		out = append(out, prefixed("query parameter "+k, Validate(s, query[k]))...)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Request.Headers)) {
		if r.Request.Headers[name].Required && headerValue(headers, name) == "" {
			out = append(out, "missing required header "+name)
		}
	}
	want := strings.Split(strings.Trim(r.Path, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	for i := range want {
		if i >= len(got) || !strings.HasPrefix(want[i], "{") {
			continue
		}
		name := strings.Trim(want[i], "{}")
		if s, ok := r.Request.Params[name]; ok {
			out = append(out, prefixed("path parameter "+name, Validate(s, got[i]))...)
		}
	}
	switch {
	case r.Request.Body == nil && len(body) > 0:
		out = append(out, "the route takes no body")
	case r.Request.Body != nil && len(body) == 0:
		if !r.Request.OptionalBody {
			out = append(out, "the route requires a body")
		}
	case r.Request.Body != nil:
		v, err := Decode(body)
		if err != nil {
			return append(out, "body is not JSON: "+err.Error())
		}
		out = append(out, prefixed("body", Validate(r.Request.Body, v))...)
	}
	return out
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// IsJSON reports whether a content type is JSON.
func IsJSON(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(mt) == "application/json"
}

func (w World) check() []string {
	var out []string
	if w.Capability == "" || w.ConversationID == "" {
		out = append(out, "world: capability and conversationId are required")
	}
	if w.LauncherEnv["AGENT_NAME"] == "" {
		out = append(out, "world: launcherEnv.AGENT_NAME is required")
	}
	for name, raw := range map[string]json.RawMessage{"tools": w.Tools, "toolsAfterChange": w.ToolsAfterChange} {
		if _, err := Decode(raw); err != nil {
			out = append(out, "world."+name+": "+err.Error())
		}
	}
	for _, name := range []string{UpstreamTokenService, UpstreamBFF, UpstreamLangfuse, UpstreamPeer} {
		for key, c := range w.Upstreams[name] {
			if c.Status == 0 {
				out = append(out, fmt.Sprintf("world.upstreams.%s[%s]: status is required", name, key))
			}
			if len(c.Body) > 0 {
				if _, err := Decode(c.Body); err != nil {
					out = append(out, fmt.Sprintf("world.upstreams.%s[%s]: %v", name, key, err))
				}
			}
		}
	}
	return out
}
