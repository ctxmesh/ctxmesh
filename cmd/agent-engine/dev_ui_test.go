// dev_ui_test.go covers `agent-engine dev --ui` (ADR 0021): the local dev-mode
// console substrate. The BFF's honest degrade (nil cluster → 501) is tested in
// internal/bff; here we prove the CLI wiring — the flag guard, the missing-build
// error, and that serveDevUI actually binds, serves the dev-mode probe + the SPA,
// and degrades cluster surfaces honestly with no cluster attached.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDev_UIWithNoWaitRejected(t *testing.T) {
	// --ui needs the run to stay up to serve the console; --no-wait exits right after
	// `up`. The two are mutually exclusive and must fail early (before any docker call).
	err := runDev(
		context.Background(),
		devFlagValues{file: "agent.yaml", port: 8080, provider: "mock", ui: true, noWait: true},
		io.Discard, io.Discard,
	)
	assertValidationErr(t, err)
}

func TestServeDevUI_MissingBuildErrors(t *testing.T) {
	// A non-existent dist dir → a clear validation error pointing at `make build-ui`,
	// not a panic or a half-started server.
	_, err := serveDevUI(0, filepath.Join(t.TempDir(), "does-not-exist"), &devPlan{}, io.Discard, io.Discard)
	assertValidationErr(t, err)
}

func TestServeDevUI_ServesConsoleAndDegrades(t *testing.T) {
	dist := t.TempDir()
	const marker = "<!doctype html><title>agent-engine console</title>"
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"), []byte(marker), 0o600))

	port := freePort(t)
	shutdown, err := serveDevUI(port, dist, &devPlan{HostPort: 8080}, io.Discard, io.Discard)
	require.NoError(t, err)
	defer shutdown()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// The dev-mode probe is unauthenticated and reports true so the SPA renders dev
	// chrome instead of the login gate.
	body, code := httpGet(t, base+"/api/devmode")
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"devMode":true}`, body)

	// Health is up.
	_, code = httpGet(t, base+"/api/health")
	assert.Equal(t, http.StatusOK, code)

	// A cluster-backed surface degrades honestly (no cluster in dev mode) — an honest
	// 501, never a crash or fabricated data.
	_, code = httpGet(t, base+"/api/agents")
	assert.Equal(t, http.StatusNotImplemented, code)

	// The SPA is served at the root.
	body, code = httpGet(t, base+"/")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "agent-engine console")
}

// freePort asks the kernel for an unused TCP port, closes the listener, and returns
// the port for serveDevUI to bind. A brief reuse race is acceptable in a unit test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func httpGet(t *testing.T, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b), resp.StatusCode
}
