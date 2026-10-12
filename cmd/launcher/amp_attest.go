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

package main

// Attested AMP (ADR 0153): who may reach a registry member's /invoke, and how a caller proves who it is.
//
// A caller's launcher trades its pod's BFF-audience token for an attestation naming its agent, registry and
// role and the agent it is about to call, and sends it with the envelope. A member admits /invoke only as:
//
//  1. its own async consumer's loopback call, marked with a secret that exists only in this process;
//  2. an attested AMP call: the attestation verifies, names this agent as its receiver and this namespace,
//     and the envelope's sender, registry and role are the attested ones;
//  3. a request carrying a run capability minted for this agent: the console, the BFF's runs and
//     workflows, and the edge. Each of those mints a fresh, unbound capability per request, so only an
//     unbound capability is admitted, and only the first time it is presented.
//
// Anything else is refused under the require posture. The accept posture (AMP_REQUIRE_ATTESTATION=false)
// also admits what was admitted before, for a two-step rollout; a caller that presents an attestation is
// held to it under either posture, so the flag never turns a bad attestation into a pass.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

const (
	ampAttestationPath = "/api/internal/amp/attestation"
	// ampAttestationFetchTimeout bounds one exchange at the BFF, inside the hop's own deadline.
	ampAttestationFetchTimeout = 5 * time.Second
	// ampAttestationRefreshMargin is how long before expiry an attestation is replaced, so a hop never
	// carries one that lapses in flight or at a callee whose clock runs ahead.
	ampAttestationRefreshMargin = time.Minute
	// maxCachedAttestations bounds the per-receiver cache. Receivers are agents in one namespace.
	maxCachedAttestations = 256

	// ampLoopbackHeader marks this launcher's own async consumer calling its /invoke. Its value is a
	// secret generated per process and stripped before the agent's code sees the request.
	ampLoopbackHeader = "X-Ctxmesh-AMP-Loopback"

	// errAttestationUnavailable: this agent could not get its attestation, so the hop was not sent.
	errAttestationUnavailable = "attestation_unavailable"
	// errAdmissionUnavailable: this member could not record a run capability as spent, so it admitted
	// nothing. Retryable.
	errAdmissionUnavailable = "admission_unavailable"

	// maxSpentCapabilities bounds the run capabilities a member remembers as spent. Only a capability the
	// BFF minted for this agent is recorded, and only until it would stop verifying (minutes), so reaching
	// the bound takes more invokes within one capability lifetime than one pod serves.
	maxSpentCapabilities = 1 << 16
	minSpentSweep        = 1024

	envAMPRequireAttestation = "AMP_REQUIRE_ATTESTATION"
)

// requireAttestation reads AMP_REQUIRE_ATTESTATION: only an explicit "false" accepts unattested calls.
func requireAttestation(raw string) bool {
	return !strings.EqualFold(strings.TrimSpace(raw), "false")
}

// capabilityVerifierFromEnv builds the verifier for attestations and run capabilities from the platform
// public key. nil, nil when the pod has no key: a member then admits only its own loopback calls, and an
// accept-posture member what it admitted before.
func capabilityVerifierFromEnv(lookup func(string) string) (*runcap.Verifier, error) {
	pubB64 := strings.TrimSpace(lookup("MCP_CAPABILITY_PUBLIC_KEY"))
	if pubB64 == "" {
		return nil, nil
	}
	pub, err := runcap.DecodePublicKey(pubB64)
	if err != nil {
		return nil, fmt.Errorf("MCP_CAPABILITY_PUBLIC_KEY: %w", err)
	}
	aud := strings.TrimSpace(lookup("MCP_CAPABILITY_AUDIENCE"))
	if aud == "" {
		aud = runcap.DefaultAudience
	}
	return runcap.NewVerifier(pub, aud, nil), nil
}

var (
	loopbackOnce   sync.Once
	loopbackSecret string
)

// processLoopbackSecret is this process's loopback marker, shared by the async consumer and the guard.
// Every agent request reaches the launcher from 127.0.0.1 through Knative's queue-proxy, so the source
// address cannot tell the consumer apart; only a value nothing outside this process knows can.
func processLoopbackSecret() string {
	loopbackOnce.Do(func() {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			// No secret means no loopback admission: async deliveries are refused rather than admitted
			// on a guessable value.
			fmt.Fprintf(os.Stderr, "launcher: AMP: no loopback secret (%v); async deliveries will be refused\n", err)
			return
		}
		loopbackSecret = hex.EncodeToString(b)
	})
	return loopbackSecret
}

// ── caller side ─────────────────────────────────────────────────────────────

type cachedAttestation struct {
	token     string
	refreshAt time.Time
}

// attestationSource fetches and caches this agent's attestations, one per receiver.
type attestationSource struct {
	url       string
	readToken func() string
	hc        *http.Client
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cachedAttestation
}

// newAttestationSource returns nil without a BFF: every hop then fails, typed, rather than going out
// unattested.
func newAttestationSource(bffURL string, readToken func() string) *attestationSource {
	bffURL = strings.TrimRight(strings.TrimSpace(bffURL), "/")
	if bffURL == "" {
		return nil
	}
	return &attestationSource{
		url:       bffURL + ampAttestationPath,
		readToken: readToken,
		hc:        &http.Client{Timeout: ampAttestationFetchTimeout, CheckRedirect: refuseRedirect},
		now:       time.Now,
		cache:     map[string]cachedAttestation{},
	}
}

// get returns an attestation for calling receiver, from the cache while it is fresh.
func (a *attestationSource) get(ctx context.Context, receiver string) (string, error) {
	if a == nil {
		return "", errors.New("no BFF to attest this agent (BFF_INTERNAL_URL is unset)")
	}
	receiver = strings.ToLower(receiver)
	a.mu.Lock()
	c, ok := a.cache[receiver]
	a.mu.Unlock()
	if ok && a.now().Before(c.refreshAt) {
		return c.token, nil
	}

	tok, lifetime, err := a.fetch(ctx, receiver)
	if err != nil {
		return "", err
	}
	margin := min(ampAttestationRefreshMargin, lifetime/2)
	now := a.now()
	a.mu.Lock()
	for k, v := range a.cache {
		if !now.Before(v.refreshAt) {
			delete(a.cache, k)
		}
	}
	if len(a.cache) >= maxCachedAttestations {
		clear(a.cache)
	}
	a.cache[receiver] = cachedAttestation{token: tok, refreshAt: now.Add(lifetime - margin)}
	a.mu.Unlock()
	return tok, nil
}

func (a *attestationSource) fetch(ctx context.Context, receiver string) (string, time.Duration, error) {
	podToken := a.readToken()
	if podToken == "" {
		return "", 0, errors.New("this pod has no BFF token (BFF_POD_TOKEN_PATH)")
	}
	body, err := json.Marshal(map[string]string{"receiver": receiver})
	if err != nil {
		return "", 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, ampAttestationFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+podToken)
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("asking the BFF: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("the BFF refused (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var out struct {
		Attestation string `json:"attestation"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decoding the BFF's answer: %w", err)
	}
	if out.Attestation == "" || out.ExpiresIn <= 0 {
		return "", 0, errors.New("the BFF answered without an attestation")
	}
	return out.Attestation, time.Duration(out.ExpiresIn) * time.Second, nil
}

// ── callee side ─────────────────────────────────────────────────────────────

// Values of the amp.admission span attribute: how a member admitted a request. The accept-to-require
// flip is safe once "unattested" stops appearing.
const (
	admissionLoopback   = "loopback"
	admissionAttested   = "attested"
	admissionCapability = "capability"
	admissionUnattested = "unattested"
)

// isLoopback reports whether r carries this process's loopback secret.
func (g *ampGuard) isLoopback(r *http.Request) bool {
	got := r.Header.Get(ampLoopbackHeader)
	return g.loopback != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(g.loopback)) == 1
}

// capabilityVerdict is what the run capability on a plain /invoke decides.
type capabilityVerdict int

const (
	// capabilityNone: no capability, or one that is not this agent's. The posture decides.
	capabilityNone capabilityVerdict = iota
	// capabilityAdmits: a fresh capability minted for this agent, now spent.
	capabilityAdmits
	// capabilityRefused: refused under either posture, and the response is written. The capability names
	// this agent but is a copy, or could not be recorded as spent.
	capabilityRefused
)

// admitCapability decides r's run capability. It admits one minted for this agent that is unbound and
// presented for the first time. The launcher binds every capability it admits to its own key before the
// agent's code sees it, so a bound one naming this agent is one that code relayed (an AMP hop carries it)
// or leaked; and a second presentation of an unbound one is a copy. Admitting either would let whoever
// holds it invoke this agent as the user, outside allowedCallers and the hop guards, with this launcher
// spending it under its own key.
func (g *ampGuard) admitCapability(ctx context.Context, w http.ResponseWriter, r *http.Request) capabilityVerdict {
	tok := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if tok == "" || g.verifier == nil {
		return capabilityNone
	}
	c, err := g.verifier.Verify(tok)
	if err != nil {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("amp.capability_rejected", err.Error()))
		return capabilityNone
	}
	if c.Agent != g.cfg.Namespace+"/"+g.cfg.SelfName {
		return capabilityNone
	}
	switch {
	case c.KeyThumbprint != "":
		g.deny(ctx, w, "", "this run capability is bound to a holder: it was handed on, not minted for this call")
		return capabilityRefused
	case strings.TrimSpace(c.ID) == "":
		g.deny(ctx, w, "", "this run capability has no id, so a copy of it cannot be told apart")
		return capabilityRefused
	}
	first, err := g.spent.spend(c.ID, c.VerifiableUntil(), g.now())
	switch {
	case err != nil:
		w.Header().Set("Retry-After", "5")
		writeAMPError(w, http.StatusServiceUnavailable, errAdmissionUnavailable, err.Error())
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("amp.error", errAdmissionUnavailable))
		return capabilityRefused
	case !first:
		g.deny(ctx, w, "", "this run capability was already presented: every invoke carries its own")
		return capabilityRefused
	}
	return capabilityAdmits
}

// spentCapabilities is the run capabilities a member has admitted, by id, each until it would stop
// verifying. Keyed by the signed id rather than the token text, so a re-encoding of the same token is
// the same capability.
type spentCapabilities struct {
	mu      sync.Mutex
	until   map[string]time.Time
	sweepAt int
}

func newSpentCapabilities() *spentCapabilities {
	return &spentCapabilities{until: map[string]time.Time{}, sweepAt: minSpentSweep}
}

var errSpentCapabilitiesFull = errors.New("this agent holds too many unexpired run capabilities to admit another")

// spend records id until `until`, reporting false when it was already recorded. It fails closed when the
// set is full of capabilities that still verify: forgetting one would let its copy in.
func (s *spentCapabilities) spend(id string, until, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.until[id]; ok {
		return false, nil
	}
	if len(s.until) >= s.sweepAt {
		for k, u := range s.until {
			if now.After(u) {
				delete(s.until, k)
			}
		}
		s.sweepAt = min(max(2*len(s.until), minSpentSweep), maxSpentCapabilities)
	}
	if len(s.until) >= maxSpentCapabilities {
		return false, errSpentCapabilitiesFull
	}
	s.until[id] = until
	return true, nil
}

// admitAttested checks an attested AMP call: the attestation, that it is for this agent in this
// namespace, that the envelope says what it attests, and then the registry and the callee's policy.
func (g *ampGuard) admitAttested(ctx context.Context, w http.ResponseWriter, raw, attTok string) bool {
	env, err := parseIncomingEnvelope(raw)
	switch {
	case err != nil:
		g.deny(ctx, w, "", "malformed AMP envelope")
		return false
	case env == nil:
		g.deny(ctx, w, "", "an attestation without an AMP envelope is not an AMP call")
		return false
	}
	return g.admitAttestedEnvelope(ctx, w, env, attTok)
}

// admitAttestedEnvelope is admitAttested for an envelope already decoded.
func (g *ampGuard) admitAttestedEnvelope(
	ctx context.Context, w http.ResponseWriter, env *envelope, attTok string,
) bool {
	if g.verifier == nil {
		g.deny(ctx, w, "", "this agent cannot verify attestations: it has no capability public key")
		return false
	}
	att, err := g.verifier.VerifyAttestation(attTok)
	if err != nil {
		g.deny(ctx, w, "", "the caller's attestation is not valid: "+err.Error())
		return false
	}
	caller := att.Namespace + "/" + att.Agent
	if att.Namespace != g.cfg.Namespace {
		// A registry is namespaced: a caller in another namespace is in another registry, whatever its
		// registry id says.
		g.denyCrossRegistry(ctx, w, caller, att.Namespace+"/"+att.RegistryID)
		return false
	}
	if att.Receiver != g.cfg.SelfName {
		g.deny(ctx, w, caller, fmt.Sprintf("the attestation is for %q, not this agent", att.Receiver))
		return false
	}
	if env.SenderAgentID != att.Agent || env.RegistryID != att.RegistryID || env.Role != att.Role {
		g.deny(ctx, w, caller, fmt.Sprintf(
			"the envelope names sender %q in registry %q with role %q, but the caller is attested as %q in %q with role %q",
			env.SenderAgentID, env.RegistryID, env.Role, att.Agent, att.RegistryID, att.Role))
		return false
	}
	return g.checkEnvelope(ctx, w, env)
}

// admitDelivery decides an async delivery before it is deduplicated or reaches the agent. It is admitted
// as an attested call is: the sender's attestation (runcap.CloudEventAttestationHeader), made for this
// agent, must vouch for the envelope's sender, registry and role. The JetStream dispatcher attests the
// sender of every hop it delivers; a producer publishing to a Knative Broker attaches its own attestation,
// fetched from the BFF as for a synchronous hop. Without one, only the accept posture admits a delivery,
// and its envelope is then checked as it claims when it is invoked. A refusal is written as the typed 403,
// which a Knative Broker does not retry and the JetStream dispatcher acknowledges.
func (g *ampGuard) admitDelivery(ctx context.Context, w http.ResponseWriter, r *http.Request, env *envelope) bool {
	span := trace.SpanFromContext(ctx)
	if attTok := strings.TrimSpace(r.Header.Get(runcap.CloudEventAttestationHeader)); attTok != "" {
		if !g.admitAttestedEnvelope(ctx, w, env, attTok) {
			return false
		}
		span.SetAttributes(attribute.String("amp.admission", admissionAttested))
		return true
	}
	if !g.cfg.AcceptUnattested {
		g.deny(ctx, w, "", "an async AMP delivery must carry the platform's attestation of its sender")
		return false
	}
	span.AddEvent("amp.unattested_admitted")
	span.SetAttributes(attribute.String("amp.admission", admissionUnattested))
	return true
}
