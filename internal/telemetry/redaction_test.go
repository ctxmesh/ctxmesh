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

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
)

func TestPlatformAttributeKeys(t *testing.T) {
	assert.Equal(t, "agent.name", AttrAgentName)
	assert.Equal(t, "agent.version", AttrAgentVersion)
	assert.Equal(t, "agent.route", AttrAgentRoute)
}

func TestIsSensitive(t *testing.T) {
	assert.True(t, IsSensitive(AttrLLMInputMessages), "llm input is payload")
	assert.True(t, IsSensitive(AttrToolCallArguments), "tool args are payload")
	assert.False(t, IsSensitive(AttrAgentName), "agent.name is metadata, not payload")
	assert.False(t, IsSensitive("random.key"), "unknown key not sensitive")
}

func TestRedactIsPassthroughStub(t *testing.T) {
	// M3 behavior: passthrough. When this test needs to change, M11 has
	// implemented real redaction.
	in := []attribute.KeyValue{
		attribute.String(AttrLLMInputMessages, "hello secret prompt"),
		attribute.String(AttrAgentName, "demo"),
	}
	out := Redact(in)
	assert.Equal(t, in, out, "M3 Redact must be a passthrough stub")
}

func TestBasicAuthHeader(t *testing.T) {
	// pk:sk → base64("pk:sk") == "cGs6c2s="
	got := BasicAuthHeader("pk", "sk")
	assert.Equal(t, "Basic cGs6c2s=", got)
}
