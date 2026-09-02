package bff

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The "custom" provider is the console's OpenAI-compatible seam: an endpoint the
// operator supplies (a self-hosted gateway, vLLM, Ollama, an Azure deployment).
// The console had offered it since M15 and the BFF rejected it as unsupported, so a
// user picked it, typed their key, and hit a wall — a break BETWEEN two screens that
// no per-screen test could see (M153; hack/provider-parity.sh now guards the pair).

func TestProviderModels_CustomProbesTheSuppliedEndpoint(t *testing.T) {
	srv := fakeProvider(t, "mock-large", "mock-small")
	defer srv.Close()

	models, err := providerModels(context.Background(), srv.Client(), providerCustom, theTestKey, srv.URL)
	if err != nil {
		t.Fatalf("custom provider probe failed: %v", err)
	}
	if len(models) != 2 || models[0] != "mock-large" {
		t.Fatalf("got %v, want the endpoint's own model list", models)
	}
}

func TestProviderModels_CustomWithoutBaseURLIsRejected(t *testing.T) {
	// There is no public default to fall back to: the base URL IS the provider's
	// identity. Rejecting it here (400) is honest; falling back to OpenAI's endpoint
	// would silently probe a vendor the user never chose.
	_, err := providerModels(context.Background(), nil, providerCustom, theTestKey, "  ")
	var pe *providerError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a typed providerError", err)
	}
	if pe.status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", pe.status)
	}
	if pe.msg != msgCustomNeedsBaseURL {
		t.Fatalf("msg %q, want the base-URL message", pe.msg)
	}
}

func TestProviderModels_CustomRejectedKeyIs422(t *testing.T) {
	// An upstream auth failure must NOT surface as a bare 401 — the SPA reads that as
	// its OWN session expiring and logs the user out (ADR 0027). Same contract as the
	// built-in providers; asserted here because custom took a new code path to reach it.
	srv := fakeProvider(t, "mock-small")
	defer srv.Close()

	_, err := providerModels(context.Background(), srv.Client(), providerCustom, "not-the-key", srv.URL)
	var pe *providerError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a typed providerError", err)
	}
	if pe.status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 (never 401 — that logs the user out)", pe.status)
	}
}

func TestCustomURLsToleratePastedV1Suffix(t *testing.T) {
	// An operator pastes whichever base URL their gateway documents, and both forms
	// are correct for that gateway. Appending a second /v1 produces a 404 the user
	// cannot diagnose from anything the console shows them.
	for _, tc := range []struct {
		base, models, chat string
	}{
		{"http://mock:9099", "http://mock:9099/v1/models", "http://mock:9099/v1/chat/completions"},
		{"http://mock:9099/v1", "http://mock:9099/v1/models", "http://mock:9099/v1/chat/completions"},
		{"http://mock:9099/v1/", "http://mock:9099/v1/models", "http://mock:9099/v1/chat/completions"},
		{"  http://mock:9099/  ", "http://mock:9099/v1/models", "http://mock:9099/v1/chat/completions"},
	} {
		if got := customModelsURL(tc.base); got != tc.models {
			t.Errorf("customModelsURL(%q) = %q, want %q", tc.base, got, tc.models)
		}
		if got := customChatURL(tc.base); got != tc.chat {
			t.Errorf("customChatURL(%q) = %q, want %q", tc.base, got, tc.chat)
		}
	}
}

func TestChatComplete_CustomWithoutBaseURLIsRejected(t *testing.T) {
	// The same wall, one screen later: a custom provider that connects but cannot
	// generate would leave "Describe it" broken for exactly the users the connect
	// fix unblocked.
	_, err := chatComplete(context.Background(), nil, providerCustom, theTestKey, "", "model", "sys", "desc", "")
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusBadRequest {
		t.Fatalf("got %v, want a 400 providerError", err)
	}
}

func TestChatComplete_CustomDrivesTheSuppliedEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"name: from-custom"}}]}`))
	}))
	defer srv.Close()

	out, err := chatComplete(context.Background(), srv.Client(), providerCustom, theTestKey, srv.URL, "mock-small", "sys", "a refund checker", "")
	if err != nil {
		t.Fatalf("custom generation failed: %v", err)
	}
	if out != "name: from-custom" {
		t.Fatalf("got %q, want the endpoint's completion", out)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer "+theTestKey {
		t.Fatalf("auth header %q — the operator's key must reach their own endpoint", gotAuth)
	}
}
