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

package bff

import (
	"strings"
)

// sseFrame renders one Server-Sent Event: an optional `id:` line, the `event:` line, the payload as
// one `data:` line per line of it, and the blank line that dispatches the event. A client rejoins
// the data lines with "\n" (the SSE grammar), so a payload with newlines arrives intact, and an empty
// payload is still one `data:` line, so the event still dispatches. SSE cannot carry a carriage
// return (CR and CRLF end a line), so both arrive as "\n".
func sseFrame(id, event, data string) string {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	for line := range strings.SplitSeq(data, "\n") {
		// "data:" alone is an empty line of payload; the space a reader strips is written only
		// before text, so the wire (and the golden fixture) carries no trailing whitespace.
		if line == "" {
			b.WriteString("data:\n")
			continue
		}
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
