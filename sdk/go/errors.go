package ctxmesh

import (
	"errors"
	"fmt"
)

// CapabilityHeader carries the run capability. Delegation, handoff, per-user session memory,
// per-user long-term memory and per-user knowledge bases all key on it. Session memory fails
// SAFE without it — every user silently shares the agent-wide bucket instead of their own — so
// omitting it defeats an isolation control with no error to notice.
const CapabilityHeader = "X-Ctxmesh-Run-Capability"

// The SDK's error vocabulary. These are sentinels so a caller can errors.Is them rather than
// matching on strings — an agent that must behave differently when a capability is simply not
// granted needs to tell that apart from a transport failure.
var (
	// ErrNotInPod means the launcher environment is absent: this process is not running as a
	// ctxmesh agent. Returned by FromEnv rather than guessing ports.
	ErrNotInPod = errors.New("ctxmesh: not running in a ctxmesh pod (no launcher environment)")

	// ErrNotWired means the platform did not grant this capability to this agent. It is a
	// configuration answer, not a failure — the port is absent because nothing is listening.
	ErrNotWired = errors.New("ctxmesh: capability not wired for this agent")

	// ErrDenied is a 403 from the plane: the call was understood and refused. Guardrails,
	// budgets and the delegate fence all surface here.
	ErrDenied = errors.New("ctxmesh: denied by the platform")
)

// APIError carries a non-2xx response. Body is included because the launcher's refusals say
// why, and swallowing that turns a policy decision into a bare status code.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ctxmesh: %s returned %d: %s", e.Path, e.Status, e.Body)
}

// Unwrap maps 403 onto ErrDenied so `errors.Is(err, ErrDenied)` works without the caller
// knowing the status code.
func (e *APIError) Unwrap() error {
	if e.Status == 403 {
		return ErrDenied
	}
	return nil
}
