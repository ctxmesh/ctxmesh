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

package jsname_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
)

func TestAgentAddressing(t *testing.T) {
	assert.Equal(t, "ctxmesh.a2a.reg-a", jsname.RegistrySubject("reg-a"))
	assert.Equal(t, "ctxmesh.a2a.reg-a.team-ns.reviewer", jsname.AgentSubject("reg-a", "team-ns", "reviewer"))
	assert.Equal(t, "ctxmesh.a2a.*.team-ns.reviewer", jsname.AgentFilter("team-ns", "reviewer"),
		"a hop to the agent from any registry reaches its queue, so a crossing is refused where it is seen")
	assert.Equal(t, "agent_team-ns_reviewer", jsname.AgentDurable("team-ns", "reviewer"))
	assert.Equal(t, "ctxmesh.a2a.*", jsname.RegistrySubjects,
		"the registry-only form is one token deep, so it never matches an agent subject")
}

// Namespaces and agent names are DNS labels and may contain '-', so the separator must be a character
// neither can hold, or two agents would share one queue.
func TestAgentDurable_IsUnambiguous(t *testing.T) {
	assert.NotEqual(t, jsname.AgentDurable("a-b", "c"), jsname.AgentDurable("a", "b-c"))
}

// A crafted value cannot add a token or a wildcard to a subject, nor a separator NATS forbids in a
// consumer name.
func TestToken_CannotEscape(t *testing.T) {
	for _, bad := range []string{"reg.a", "*", ">", "a b", "a/b", `a\b`} {
		tok := jsname.Token(bad)
		assert.False(t, strings.ContainsAny(tok, `.*> /\`), "%q became %q", bad, tok)
	}
	assert.Equal(t, 5, strings.Count(jsname.AgentSubject("r.x", "n.y", "a.z"), ".")+1,
		"an agent subject is always five tokens")
}
