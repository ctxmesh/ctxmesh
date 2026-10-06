package bff

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The BFF runs where the chart sets MANAGED_AGENT_IMAGE, so the package's tests do too. Without it a
// managed agent has no image anyone could pull, and the authoring API refuses it (tested below).
func init() {
	if os.Getenv("MANAGED_AGENT_IMAGE") == "" {
		_ = os.Setenv("MANAGED_AGENT_IMAGE", testManagedImage)
	}
}

// A BFF with no MANAGED_AGENT_IMAGE (a hand-rolled install) refuses to preview a managed agent and says
// what to set, instead of handing back a manifest whose image can never be pulled.
func TestGenerateManagedWithoutAnImageSaysWhatToSet(t *testing.T) {
	t.Setenv("MANAGED_AGENT_IMAGE", "")
	prov, _ := fakeChatProvider(t, validGeneratedYAML)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)...).Build()
	s, _, _ := newGenerateServer(t, c, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a support bot", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "MANAGED_AGENT_IMAGE"), rec.Body.String())
}
