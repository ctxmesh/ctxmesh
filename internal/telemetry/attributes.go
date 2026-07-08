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

// Span attribute keys used across the platform. Platform keys are set by the
// launcher boundary span; the OpenInference keys are emitted by the base-image
// auto-instrumentation and are the payload-bearing (hence redactable) fields.
const (
	// Platform attributes — set on the launcher's agent.invoke boundary span.
	AttrAgentName    = "agent.name"
	AttrAgentVersion = "agent.version"
	AttrAgentRoute   = "agent.route"

	// OpenInference payload attributes — these carry prompt/response/tool data
	// and are the fields M11's redaction policy targets before persistence.
	AttrLLMInputMessages  = "llm.input_messages"
	AttrLLMOutputMessages = "llm.output_messages"
	AttrToolCallArguments = "tool.call.arguments"
	AttrToolCallResult    = "tool.call.result"
	AttrInputValue        = "input.value"
	AttrOutputValue       = "output.value"
)

// SensitiveAttributeKeys is the set of span-attribute keys that carry
// user/model payload and are therefore candidates for redaction. M3 only
// enumerates them; the actual redaction is a passthrough stub until M11
// (§13.3) implements regex/detector policy at the collector.
var SensitiveAttributeKeys = []string{
	AttrLLMInputMessages,
	AttrLLMOutputMessages,
	AttrToolCallArguments,
	AttrToolCallResult,
	AttrInputValue,
	AttrOutputValue,
}
