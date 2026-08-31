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

package egress

import (
	"errors"
	"io"
	"sync"
	"time"
)

// The mid-stream idle watchdog (M143.4, m52.G18).
//
// F3 (M126) bounded the tool forward with ResponseHeaderTimeout, which catches an MCP server that
// connects but never responds. It does NOT catch the rarer and nastier case: a streamable-http server
// that sends its headers — so the timeout is satisfied and disarmed — and then goes silent forever. The
// reverse proxy streams with FlushInterval -1 and will happily wait indefinitely for bytes that never
// come, wedging the run's managed loop exactly as a hung server would.
//
// The fix is a watchdog on the RESPONSE BODY rather than another connection-level timeout: what matters
// is time since the last byte, not total duration. A legitimate long stream keeps resetting it; a stalled
// one does not. Bounding total duration instead would cut healthy long-running tools, which is why
// F3's approach cannot simply be given a longer deadline.

// ErrStreamStalled is returned by a stalled read. It is distinct so a caller can tell "the upstream went
// silent" from "the upstream closed" — the first is a fault worth reporting, the second is a normal EOF.
var ErrStreamStalled = errors.New("egress: the MCP server went silent mid-stream (idle timeout)")

// idleTimeoutReader fails a read when no data has arrived for idle.
//
// It uses a timer armed around each Read rather than a background goroutine watching a timestamp: the
// blocking read IS the thing being bounded, so the timer must be able to interrupt it. Closing the
// underlying body is what unblocks a Read parked in the network stack — there is no way to cancel it
// otherwise — so the watchdog closes it and the parked Read returns, at which point we report the stall
// rather than the incidental "use of closed connection" error it produced.
type idleTimeoutReader struct {
	body io.ReadCloser
	idle time.Duration

	mu      sync.Mutex
	stalled bool
	closed  bool
}

// newIdleTimeoutReader wraps body so a gap longer than idle fails the read. A non-positive idle disables
// the watchdog and returns the body unchanged — an operator setting 0 means "no bound", and silently
// substituting a default would be a different decision than the one they made.
func newIdleTimeoutReader(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		return body
	}
	return &idleTimeoutReader{body: body, idle: idle}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.idle, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return
		}
		// Mark BEFORE closing: the parked Read wakes with a connection error, and without this flag we
		// would report that incidental error instead of the stall that actually happened.
		r.stalled = true
		r.closed = true
		_ = r.body.Close()
	})
	defer timer.Stop()

	n, err := r.body.Read(p)
	if err != nil {
		r.mu.Lock()
		stalled := r.stalled
		r.mu.Unlock()
		if stalled {
			return n, ErrStreamStalled
		}
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}
