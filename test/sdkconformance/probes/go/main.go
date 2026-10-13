// Command probe drives the Go SDK's public API against a real launcher, one call per contract
// route, and prints "PASS|FAIL|MISSING <route>[: reason]" for each. hack/sdk-conformance runs it
// and checks every PASS against what the launcher actually received.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	ctxmesh "github.com/ctxmesh/ctxmesh/sdk/go"
)

type golden struct {
	Request struct {
		Path  string            `json:"path"`
		Query map[string]string `json:"query"`
		Body  json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Body json.RawMessage `json:"body"`
	} `json:"response"`
}

type route struct {
	Method    string   `json:"method"`
	Path      string   `json:"path"`
	SDKResult []string `json:"sdkResult"`
	Goldens   []golden `json:"goldens"`
}

// required is the current route's sdkResult: the response fields the SDK's result must carry.
var required []string

func main() {
	raw, err := os.ReadFile(os.Getenv("CTXMESH_PROBE_CONTRACT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: read the contract:", err)
		os.Exit(2)
	}
	var contract struct {
		Routes []route `json:"routes"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		fmt.Fprintln(os.Stderr, "probe: parse the contract:", err)
		os.Exit(2)
	}
	// FromEnv, not an explicit Config: the env the launcher publishes is part of what is checked.
	c, err := ctxmesh.New()
	checks := map[string]func(golden) error{}
	if err == nil {
		checks = calls(c, os.Getenv("CTXMESH_PROBE_CAPABILITY"))
	}
	for _, r := range contract.Routes {
		key := r.Method + " " + r.Path
		check, ok := checks[key]
		switch {
		case err != nil:
			fmt.Printf("FAIL %s: the SDK does not configure from the launcher's env: %v\n", key, err)
		case !ok:
			fmt.Printf("MISSING %s: the Go SDK has no call for it\n", key)
		default:
			required = r.SDKResult
			if cerr := check(r.Goldens[0]); cerr != nil {
				fmt.Printf("FAIL %s: %s\n", key, strings.ReplaceAll(cerr.Error(), "\n", " "))
			} else {
				fmt.Printf("PASS %s\n", key)
			}
		}
	}
}

func calls(c *ctxmesh.Client, capability string) map[string]func(golden) error {
	ctx := context.Background()
	return map[string]func(golden) error{
		"POST /a2a/{targetAgent}": func(g golden) error {
			reply, err := c.Mesh.CallLegacy(ctx, segment(g.Request.Path, 1), body(g))
			return expectJSON("reply", reply, err, g.Response.Body)
		},
		"POST /amp/{targetAgent}": func(g golden) error {
			reply, err := c.Mesh.Call(ctx, segment(g.Request.Path, 1), body(g))
			return expectJSON("reply", reply, err, g.Response.Body)
		},
		"POST /delegate": func(g golden) error {
			b := obj(g.Request.Body)
			d, err := c.Runs.Delegate(ctx, str(b["subAgent"]), str(b["step"]), str(b["callId"]), capability, b["input"])
			return expectJSON("delegation", d, err, g.Response.Body)
		},
		"ANY /feedback": func(g golden) error {
			b := obj(g.Request.Body)
			return c.Feedback.Score(ctx, str(b["traceId"]), str(b["name"]), num(b["value"]), str(b["comment"]))
		},
		"POST /handoff": func(g golden) error {
			b := obj(g.Request.Body)
			h, err := c.Runs.Handoff(ctx, str(b["targetAgent"]), capability, str(b["message"]), b["includeHistory"] != false)
			return expectJSON("handoff", h, err, g.Response.Body)
		},
		"POST /knowledge/search": func(g golden) error {
			b := obj(g.Request.Body)
			chunks, err := c.Knowledge.Search(ctx, str(b["query"]), str(b["knowledgeBase"]), int(num(b["topK"])))
			return expectJSON("chunks", map[string]any{"results": chunks}, err, g.Response.Body)
		},
		"POST /memory/agent/remember": func(g golden) error {
			b := obj(g.Request.Body)
			tags := map[string]string{}
			for k, v := range obj(mustJSON(b["tags"])) {
				tags[k] = str(v)
			}
			return c.Memory.Remember(ctx, str(b["content"]), tags)
		},
		"POST /memory/agent/search": func(g golden) error {
			b := obj(g.Request.Body)
			facts, err := c.Memory.SearchAgent(ctx, str(b["query"]), int(num(b["topK"])), 0)
			return expectJSON("facts", map[string]any{"results": facts}, err, g.Response.Body)
		},
		"GET /memory/{conversationId}": func(g golden) error {
			entries, err := c.Memory.Get(ctx, segment(g.Request.Path, 1))
			return expectJSON("entries", entries, err, g.Response.Body)
		},
		"PUT /memory/{conversationId}": func(g golden) error {
			var entries []ctxmesh.Entry
			if err := json.Unmarshal(g.Request.Body, &entries); err != nil {
				return err
			}
			return c.Memory.Put(ctx, entries, segment(g.Request.Path, 1))
		},
		"POST /memory/{conversationId}/append": func(g golden) error {
			var e ctxmesh.Entry
			if err := json.Unmarshal(g.Request.Body, &e); err != nil {
				return err
			}
			return c.Memory.Append(ctx, e, segment(g.Request.Path, 1))
		},
		"GET /memory/{conversationId}/search": func(g golden) error {
			entries, err := c.Memory.Search(ctx, g.Request.Query["q"], segment(g.Request.Path, 1), "")
			return expectJSON("entries", entries, err, g.Response.Body)
		},
		"GET /skills": func(g golden) error {
			skills, err := c.Skills.List(ctx)
			return expectJSON("skills", map[string]any{"skills": skills}, err, g.Response.Body)
		},
		"POST /skills/load": func(g golden) error {
			got, err := c.Skills.Load(ctx, str(obj(g.Request.Body)["name"]))
			return expectJSON("skill", map[string]any{"body": got}, err, g.Response.Body)
		},
	}
}

// expectJSON compares what the SDK decoded with the launcher's golden answer. Every field the SDK's
// type carries must hold the launcher's value; a field the SDK does not model is not its concern.
func expectJSON(what string, got any, err error, want json.RawMessage) error {
	if err != nil {
		return err
	}
	var g, w any
	if err := json.Unmarshal(mustJSON(got), &g); err != nil {
		return err
	}
	if err := json.Unmarshal(want, &w); err != nil {
		return err
	}
	want = mustJSON(w)
	if path, ok := within(g, w, "$"); !ok {
		return fmt.Errorf("%s decoded as %s, but the launcher sent %s (differs at %s)", what, mustJSON(got), want, path)
	}
	for _, field := range required {
		if at, ok := carries(g, w, field); !ok {
			return fmt.Errorf("%s decoded as %s, which does not carry %s with the launcher's value (at %s); the launcher sent %s",
				what, mustJSON(got), field, at, want)
		}
	}
	return nil
}

// carries reports whether got holds want's value at a sdkResult path. within alone passes an SDK
// that dropped a field, because it compares only what the SDK kept.
func carries(got, want any, path string) (string, bool) {
	if path == "$" {
		return "$", reflect.DeepEqual(got, want)
	}
	return carry(got, want, strings.Split(path, "."), "$")
}

func carry(got, want any, segs []string, at string) (string, bool) {
	if len(segs) == 0 {
		return at, reflect.DeepEqual(got, want)
	}
	key, each := strings.CutSuffix(segs[0], "[]")
	if key != "" {
		g, _ := got.(map[string]any)
		w, _ := want.(map[string]any)
		gv, present := g[key]
		if !present {
			return at + "." + key, false
		}
		got, want, at = gv, w[key], at+"."+key
	}
	if !each {
		return carry(got, want, segs[1:], at)
	}
	ga, _ := got.([]any)
	wa, _ := want.([]any)
	if len(ga) != len(wa) {
		return at, false
	}
	for i := range wa {
		if p, ok := carry(ga[i], wa[i], segs[1:], fmt.Sprintf("%s[%d]", at, i)); !ok {
			return p, false
		}
	}
	return at, true
}

// within reports whether every value in got appears, equal, at the same place in want, and whether
// arrays have the same length. It returns where they first differ.
func within(got, want any, at string) (string, bool) {
	switch gv := got.(type) {
	case map[string]any:
		wv, ok := want.(map[string]any)
		if !ok {
			return at, false
		}
		for k, v := range gv {
			if isZero(v) {
				if _, present := wv[k]; !present {
					continue
				}
			}
			if p, ok := within(v, wv[k], at+"."+k); !ok {
				return p, false
			}
		}
		return "", true
	case []any:
		wv, ok := want.([]any)
		if !ok || len(wv) != len(gv) {
			return at, false
		}
		for i := range gv {
			if p, ok := within(gv[i], wv[i], fmt.Sprintf("%s[%d]", at, i)); !ok {
				return p, false
			}
		}
		return "", true
	}
	return at, reflect.DeepEqual(got, want)
}

func isZero(v any) bool {
	return v == nil || v == "" || v == false || v == 0.0 || reflect.DeepEqual(v, map[string]any{})
}

func segment(path string, i int) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

func body(g golden) any {
	var v any
	_ = json.Unmarshal(g.Request.Body, &v)
	return v
}

func obj(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
