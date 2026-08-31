package egress

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingBody yields its chunks, then blocks forever — a streamable-http MCP server that sent headers
// and some data and then went silent, which is exactly what F3's header timeout cannot catch.
type blockingBody struct {
	mu     sync.Mutex
	chunks []string
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody(chunks ...string) *blockingBody {
	return &blockingBody{chunks: chunks, closed: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.chunks) > 0 {
		c := b.chunks[0]
		b.chunks = b.chunks[1:]
		b.mu.Unlock()
		return copy(p, c), nil
	}
	b.mu.Unlock()
	<-b.closed // silence, until someone closes us
	return 0, errors.New("read on closed body")
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// THE G18 CASE: a stream that delivers data and then goes silent is CUT, with a typed stall error — not
// left hanging. Without the watchdog this read never returns.
func TestIdleWatchdog_CutsAStreamThatGoesSilent(t *testing.T) {
	body := newBlockingBody("event: hello\n\n")
	r := newIdleTimeoutReader(body, 100*time.Millisecond)

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	require.NoError(t, err, "the first chunk arrives normally")
	assert.Equal(t, "event: hello\n\n", string(buf[:n]))

	start := time.Now()
	_, err = r.Read(buf)
	assert.ErrorIs(t, err, ErrStreamStalled,
		"a silent upstream must surface as a STALL, not as an incidental closed-connection error")
	assert.Less(t, time.Since(start), 2*time.Second, "and must be cut promptly, not left to hang")
}

// A healthy stream keeps resetting the clock: the bound is on the GAP between bytes, not total duration.
// Bounding total duration instead would cut long-running-but-alive tools, which is why F3 could not just
// be given a longer deadline.
func TestIdleWatchdog_ASteadyStreamIsNeverCut(t *testing.T) {
	pr, pw := io.Pipe()
	r := newIdleTimeoutReader(pr, 150*time.Millisecond)
	defer func() { _ = r.Close() }()

	go func() {
		for range 6 { // ~300ms total — well past the idle bound, but never idle
			time.Sleep(50 * time.Millisecond)
			_, _ = pw.Write([]byte("tick\n"))
		}
		_ = pw.Close()
	}()

	got, err := io.ReadAll(r)
	require.NoError(t, err, "a stream that keeps producing must never be cut, however long it runs")
	assert.Equal(t, 6, strings.Count(string(got), "tick"))
}

// A normal EOF is a normal EOF — the watchdog must not turn an upstream's clean close into a fault.
func TestIdleWatchdog_CleanCloseIsNotAStall(t *testing.T) {
	r := newIdleTimeoutReader(io.NopCloser(strings.NewReader("done")), time.Second)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "done", string(got))
}

// A non-positive idle disables the watchdog and hands back the body untouched: an operator asking for no
// bound gets no bound, rather than a default silently substituted for their decision.
func TestIdleWatchdog_ZeroDisablesIt(t *testing.T) {
	body := newBlockingBody("x") // a pointer, so identity is comparable
	assert.Same(t, body, newIdleTimeoutReader(body, 0), "zero returns the body unwrapped")
	assert.Same(t, body, newIdleTimeoutReader(body, -1))
}

// Closing twice is safe — the reverse proxy and the recorder both close, and a double-close must not
// panic or mask the first result.
func TestIdleWatchdog_CloseIsIdempotent(t *testing.T) {
	r := newIdleTimeoutReader(newBlockingBody("a"), time.Second)
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())
}
