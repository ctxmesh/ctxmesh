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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// TestValidateFeedbackStoreSpec pins the two invariants the BFF depends on (ADR 0112 §3): at least one
// source is declared, and score names are UNIQUE across all sources (the attribution key).
func TestValidateFeedbackStoreSpec(t *testing.T) {
	human := func(names ...string) *agentsv1beta1.HumanSource {
		s := &agentsv1beta1.HumanSource{}
		for _, n := range names {
			s.Scores = append(s.Scores, agentsv1beta1.ScoreDecl{Name: n})
		}
		return s
	}
	ext := func(channel, score string) agentsv1beta1.ExternalSource {
		return agentsv1beta1.ExternalSource{Name: channel, Score: agentsv1beta1.ScoreDecl{Name: score}}
	}

	cases := []struct {
		name    string
		spec    agentsv1beta1.FeedbackStoreSpec
		wantErr bool
	}{
		{"no sources at all", agentsv1beta1.FeedbackStoreSpec{}, true},
		{"human with zero scores + no external", agentsv1beta1.FeedbackStoreSpec{Human: &agentsv1beta1.HumanSource{}}, true},
		{"coherent human only", agentsv1beta1.FeedbackStoreSpec{Human: human("thumbs", "accuracy")}, false},
		{"coherent external only", agentsv1beta1.FeedbackStoreSpec{External: []agentsv1beta1.ExternalSource{ext("csat-webhook", "csat")}}, false},
		{"coherent human + external", agentsv1beta1.FeedbackStoreSpec{
			Human: human("thumbs"), External: []agentsv1beta1.ExternalSource{ext("csat-webhook", "csat")},
		}, false},
		{"duplicate name within human", agentsv1beta1.FeedbackStoreSpec{Human: human("dup", "dup")}, true},
		{"duplicate name across human and external", agentsv1beta1.FeedbackStoreSpec{
			Human: human("shared"), External: []agentsv1beta1.ExternalSource{ext("ch", "shared")},
		}, true},
		{"duplicate name across two external channels", agentsv1beta1.FeedbackStoreSpec{
			External: []agentsv1beta1.ExternalSource{ext("ch1", "same"), ext("ch2", "same")},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateFeedbackStoreSpec(&tc.spec)
			if tc.wantErr {
				assert.NotEmpty(t, msg, "expected an invalid-spec message")
			} else {
				assert.Empty(t, msg, "expected a coherent spec, got: %s", msg)
			}
		})
	}
}
