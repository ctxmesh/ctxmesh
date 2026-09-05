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

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Skills on the launcher plane (ADR 0137).
//
// Progressive disclosure is about what enters the MODEL's context, not what sits on disk. So
// the split here is deliberate and asymmetric:
//
//	GET  /skills       names + descriptions, served from injected env — NO network, no I/O.
//	                   This is called on every run, so it must stay free.
//	POST /skills/load  one body, read from the mounted directory the controller populated.
//
// Descriptions ride in env because they are always-on context anyway and are bounded (1024
// bytes × 16 skills). Bodies are mounted rather than fetched because a network call at the
// moment a model asks for a skill is a latency spike in the middle of a turn, and because the
// content is already pinned by digest — there is nothing to re-fetch.
const (
	// envSkillRefs is the comma-separated pinned refs, "<name>@sha256:…".
	envSkillRefs = "SKILL_REFS"
	// envSkillDescriptions is a JSON object of name → description.
	envSkillDescriptions = "SKILL_DESCRIPTIONS"
	// envSkillDir is the directory the controller mounts skill bodies into, one file per
	// skill name. Empty when no skills are attached.
	envSkillDir = "SKILL_DIR"
)

type skillEntry struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	Description string `json:"description"`
}

type skillServer struct {
	entries []skillEntry
	dir     string
}

// newSkillServer reads the injected env. Returns nil when no skills are attached, so the
// endpoints are not registered at all — an agent without skills gets a 404, which is the
// honest answer, rather than an empty list that reads as "you have none configured".
func newSkillServer() *skillServer {
	raw := strings.TrimSpace(os.Getenv(envSkillRefs))
	if raw == "" {
		return nil
	}
	descriptions := map[string]string{}
	if d := strings.TrimSpace(os.Getenv(envSkillDescriptions)); d != "" {
		// A malformed descriptions blob must not take the whole surface down: the refs are
		// the load-bearing part, and a skill with a missing description is still loadable.
		_ = json.Unmarshal([]byte(d), &descriptions)
	}

	var entries []skillEntry
	for ref := range strings.SplitSeq(raw, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		name, digest, found := strings.Cut(ref, "@")
		if !found {
			// The controller only ever writes pinned refs. An unpinned one means something
			// upstream is wrong, and silently accepting it would let an agent run against
			// content nobody can identify.
			continue
		}
		entries = append(entries, skillEntry{
			Name: name, Digest: digest, Description: descriptions[name],
		})
	}
	if len(entries) == 0 {
		return nil
	}
	return &skillServer{entries: entries, dir: strings.TrimSpace(os.Getenv(envSkillDir))}
}

func (s *skillServer) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /skills", s.handleList)
	mux.HandleFunc("POST /skills/load", s.handleLoad)
}

// handleList returns names, digests and descriptions — never bodies. Zero I/O.
func (s *skillServer) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"skills": s.entries})
}

// handleLoad returns ONE skill's body.
func (s *skillServer) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)

	// The name must be one of THIS agent's attached skills. This is the same un-forgeable
	// roster gate the knowledge and delegate endpoints use: what the agent may reach is
	// decided by what the controller injected, never by what the agent asks for.
	var known bool
	for _, e := range s.entries {
		if e.Name == name {
			known = true
			break
		}
	}
	if !known {
		writeJSONError(w, http.StatusForbidden, "skill "+name+" is not attached to this agent")
		return
	}
	if s.dir == "" {
		writeJSONError(w, http.StatusNotImplemented, "skill bodies are not mounted on this agent")
		return
	}
	// filepath.Base defends the mount against a traversing name. The roster check above
	// already makes one impossible, but a path built from request input should not depend on
	// a check somewhere else staying correct.
	body, err := os.ReadFile(filepath.Join(s.dir, filepath.Base(name)))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "skill "+name+" has no mounted body")
		return
	}
	writeJSON(w, map[string]any{"body": string(body)})
}
