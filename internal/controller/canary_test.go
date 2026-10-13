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
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
)

func ksvcWith(template string, spec, status []servingv1.TrafficTarget) *servingv1.Service {
	k := &servingv1.Service{}
	k.Spec.Template.Name = template
	k.Spec.Traffic = spec
	k.Status.Traffic = status
	return k
}

// The old arm is the stable arm of what serves, never the template of a candidate that serves nothing.
func TestOldServingRevision(t *testing.T) {
	latest := true
	full := int64(100)
	defaulted := []servingv1.TrafficTarget{{LatestRevision: &latest, Percent: &full}}
	cases := []struct {
		name      string
		ksvc      *servingv1.Service
		candidate string
		want      string
	}{
		{"no service", nil, "c", ""},
		{"implicit traffic serves the template", ksvcWith("a", nil, nil), "c", "a"},
		{"implicit traffic, template is the candidate", ksvcWith("c", nil, nil), "c", ""},
		{"split", ksvcWith("c", namedSplitTraffic("a", "c", 10), nil), "c", "a"},
		{
			"a new candidate splits against the stable arm, not one past 50%",
			ksvcWith("b", namedSplitTraffic("a", "b", 60), nil), "c", "a",
		},
		{"a revert mid-split has no old arm", ksvcWith("b", namedSplitTraffic("a", "b", 10), nil), "a", ""},
		{
			"after an abort, a new candidate splits against the kept revision",
			ksvcWith("rejected", namedFullTraffic("a"), nil), "c", "a",
		},
		{
			"after an abort, reverting to the kept revision has no old arm",
			ksvcWith("rejected", namedFullTraffic("a"), nil), "a", "",
		},
		{
			"the configured block wins over a status that lags it",
			ksvcWith("rejected", namedFullTraffic("a"), namedSplitTraffic("rejected", "a", 90)), "a", "",
		},
		{
			"Knative's defaulted block names nothing, so status decides",
			ksvcWith("b", defaulted, namedFullTraffic("a")), "b", "a",
		},
		{"status decides over the template", ksvcWith("rejected", nil, namedFullTraffic("a")), "a", ""},
		{"status names the old arm", ksvcWith("c", nil, namedFullTraffic("a")), "c", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, oldServingRevision(tc.ksvc, tc.candidate))
		})
	}
}

// The stable arm is the first named entry, which every block the controller writes puts first.
func TestStableArm(t *testing.T) {
	latest := true
	full := int64(100)
	assert.Equal(t, "a", stableArm(namedFullTraffic("a")))
	assert.Equal(t, "a", stableArm(namedSplitTraffic("a", "b", 90)), "the old arm, whatever the candidate's percent")
	assert.Empty(t, stableArm([]servingv1.TrafficTarget{{LatestRevision: &latest, Percent: &full}}))
	assert.Empty(t, stableArm(nil))
}
