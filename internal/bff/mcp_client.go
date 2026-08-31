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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file implements the SERVER-SIDE MCP probe for the BYO-MCP register flow
// (ADR 0016). It speaks the minimal streamable-HTTP MCP handshake — initialize →
// notifications/initialized → tools/list — against a user-registered MCP server
// and captures each discovered tool with its name, description, and inputSchema
// (the JSON Schema the managed loop, m14.6b, needs to hand the model exact
// tool-call shapes). The optional bearer key is attached to these calls
// SERVER-SIDE only (the egress hop); it is never returned to the browser and
// never logged — the same key-discipline as the m14.4 provider probe.
//
// The wire sequence mirrors the SDK's Python client (sdk/python/src/ctxmesh/
// tools.py) so a server that answers the SDK answers this probe byte-for-byte:
//   1. POST initialize                 → result + an Mcp-Session-Id response header
//   2. POST notifications/initialized  (a notification: no id, no response body)
//   3. POST tools/list                 → the tools with name/description/inputSchema
// A streamable-http server may reply application/json OR a text/event-stream SSE
// frame ("data: <json>"); both are parsed.

// mcpProtocolVersion is the MCP protocol version the probe negotiates on
// initialize. Kept in lockstep with the SDK client (_MCP_PROTOCOL_VERSION).
const mcpProtocolVersion = "2025-03-26"

// JSON-RPC / MCP method + version literals, hoisted to constants so the three
// handshake calls speak the same words (and to satisfy goconst).
const (
	jsonRPCVersion         = "2.0"
	methodInitialize       = "initialize"
	methodInitialized      = "notifications/initialized"
	methodToolsList        = "tools/list"
	contentTypeEventStream = "text/event-stream"
)

// defaultMCPTimeout bounds the whole probe so a slow or hostile MCP endpoint
// cannot hang a BFF request. The three round-trips share this deadline via the
// request context.
const defaultMCPTimeout = 10 * time.Second

// maxMCPBodyBytes bounds each MCP response body so a hostile server cannot force
// unbounded buffering. Tool catalogs (names + schemas) are small; 1 MiB is
// generous.
const maxMCPBodyBytes = 1 << 20

// mcpError is a typed probe failure carrying the HTTP status the handler should
// surface. A server that is unreachable or does not speak MCP is the CALLER'S
// mistake (they pasted a bad URL / a non-MCP endpoint), so it maps to a 4xx with
// a teaching message — never a 500 on us, and never a swallowed success. The
// message is client-safe and NEVER contains the bearer key.
type mcpError struct {
	status int
	msg    string
}

func (e *mcpError) Error() string { return e.msg }

// isMCPError reports whether err is an *mcpError and returns it so the handler
// can map its status; a nil/other error returns (nil,false).
func isMCPError(err error) (*mcpError, bool) {
	var me *mcpError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// discoveredTool is one tool captured from an MCP server's tools/list. Name is
// the catalog key (the tools/call match key); Description is advisory; InputSchema
// is the raw JSON Schema bytes (the object under the tool's "inputSchema"),
// captured verbatim so the managed loop can plumb it to the model unaltered.
type discoveredTool struct {
	Name        string
	Description string
	// InputSchema is the raw JSON of the tool's inputSchema object, or nil when
	// the server advertised none. Stored verbatim (json.RawMessage) — never
	// re-marshaled — so no schema detail is lost.
	InputSchema json.RawMessage
}

// mcpToolsListResult is the shape of a tools/list result. We map only the fields
// we surface and keep InputSchema as raw JSON so an unknown schema keyword is
// preserved rather than dropped.
type mcpToolsListResult struct {
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
}

// jsonRPCResponse is the envelope of a JSON-RPC 2.0 response. Result and Error
// are raw so we decode the result only when there is no error.
type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// jsonRPCRequest is the JSON-RPC 2.0 request the probe sends. A request with a
// zero ID (a notification, e.g. notifications/initialized) omits both id and
// params on the wire and expects no response body.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// probeMCPServer runs the MCP handshake against url and returns the discovered
// tools with their inputSchema. apiKey, when non-empty, is attached as a Bearer
// token on every call (SERVER-SIDE only). httpClient lets tests point the probe
// at an httptest MCP server; nil uses a bounded default client.
//
// Every failure is a typed *mcpError with a 4xx/502 status and a teaching,
// key-free message: an unreachable host → 502 (upstream fault), a non-MCP or
// mis-speaking endpoint → 400/422 (the caller's bad URL), a server that answers
// but advertises no tools → 422. The bearer key is used only to authenticate the
// probe; it is neither returned nor logged.
func probeMCPServer(ctx context.Context, httpClient *http.Client, url, apiKey string) ([]discoveredTool, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, &mcpError{status: http.StatusBadRequest, msg: "url is required to probe an MCP server"}
	}

	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: defaultMCPTimeout}
	}
	// Bound the whole handshake even when the injected client has no timeout.
	ctx, cancel := context.WithTimeout(ctx, defaultMCPTimeout)
	defer cancel()

	// 1. initialize → negotiate + capture the session id (some servers require it
	// on subsequent calls).
	initResult, sessionID, err := mcpPost(ctx, c, url, apiKey, "", jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      1,
		Method:  methodInitialize,
		Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "ctxmesh-bff", "version": "0.1.0"}, //nolint:goconst
		},
	})
	if err != nil {
		return nil, err
	}
	_ = initResult // negotiated capabilities are unused for discovery

	// 2. notifications/initialized (a notification: no id, no response expected).
	if _, _, err := mcpPost(ctx, c, url, apiKey, sessionID, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		Method:  methodInitialized,
	}); err != nil {
		return nil, err
	}

	// 3. tools/list → the tools with their inputSchema.
	listResult, _, err := mcpPost(ctx, c, url, apiKey, sessionID, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      2,
		Method:  methodToolsList,
		Params:  map[string]any{},
	})
	if err != nil {
		return nil, err
	}

	return parseToolsList(listResult)
}

// parseToolsList decodes a tools/list result into discoveredTools, capturing each
// tool's inputSchema verbatim. A result with no tools array or an empty tool list
// is a 422 (the server speaks MCP but advertises nothing to bind).
func parseToolsList(result json.RawMessage) ([]discoveredTool, error) {
	if len(bytes.TrimSpace(result)) == 0 {
		return nil, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server returned no tools/list result",
		}
	}
	var parsed mcpToolsListResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server's tools/list result was not valid",
		}
	}
	out := make([]discoveredTool, 0, len(parsed.Tools))
	for _, t := range parsed.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		var schema json.RawMessage
		if len(bytes.TrimSpace(t.InputSchema)) > 0 {
			schema = t.InputSchema
		}
		out = append(out, discoveredTool{
			Name:        name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	if len(out) == 0 {
		return nil, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server advertises no tools (nothing to add)",
		}
	}
	return out, nil
}

// mcpPost sends one JSON-RPC message and returns (result, newSessionID). A
// notification (req.ID == 0, e.g. notifications/initialized) does not require a
// response body. The bearer key, when non-empty, is set on the Authorization
// header here — the ONE place the key crosses onto the wire, server-side only.
// Transport failures → 502; an MCP-level JSON-RPC error or a non-2xx → 400/422
// (the caller's endpoint is wrong). The key is never logged and never placed in
// an error message.
func mcpPost(ctx context.Context, c *http.Client, url, apiKey, sessionID string, payload jsonRPCRequest) (json.RawMessage, string, error) {
	isNotification := payload.ID == 0
	body, err := json.Marshal(payload)
	if err != nil {
		// Our own payload failed to marshal — a programming error, not the caller's.
		return nil, "", &mcpError{status: http.StatusInternalServerError, msg: "failed to build the MCP request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// A malformed URL the caller pasted → an honest 400, not a 500.
		return nil, "", &mcpError{status: http.StatusBadRequest, msg: "the MCP server URL is not valid"}
	}
	req.Header.Set("Content-Type", "application/json")
	// A streamable-http server may reply with either representation.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if apiKey != "" {
		// The bearer key is attached SERVER-SIDE at this egress hop only. It is
		// never in a DTO and never logged (ADR 0016).
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.Do(req)
	if err != nil {
		// Unreachable host / transport failure → an upstream 502. The transport
		// error can carry the URL but never the key; we surface a generic message.
		return nil, "", &mcpError{
			status: http.StatusBadGateway,
			msg:    "could not reach the MCP server (check the URL and that it is running)",
		}
	}
	defer func() { _ = resp.Body.Close() }()

	newSession := resp.Header.Get("Mcp-Session-Id")

	// A notification expects only an ack (200/202) and no body. Accept and return.
	if isNotification {
		return nil, newSession, nil
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The TARGET MCP server rejected the credential (or requires one). This must
		// NOT surface as a 401 from the BFF: the SPA treats any mid-session 401 as
		// the CALLER's own session expiring and logs them out (api.ts). Return 422
		// (a probe validation failure) with a clear message so the add-MCP form shows
		// a probe error instead of kicking the user to the login screen (M23/U-mcp-401).
		return nil, newSession, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server requires authentication or rejected the credential — provide a valid bearer key (or use OAuth)",
		}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, newSession, &mcpError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("the MCP server returned an unexpected status %d", resp.StatusCode),
		}
	}

	msg, err := parseJSONRPC(resp)
	if err != nil {
		return nil, newSession, err
	}
	if msg.Error != nil {
		// A JSON-RPC error from the server (e.g. method not found on a non-MCP
		// endpoint) → a 422: the endpoint answered but is not a working MCP server.
		return nil, newSession, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server reported an error during discovery (is this an MCP endpoint?)",
		}
	}
	return msg.Result, newSession, nil
}

// parseJSONRPC extracts the JSON-RPC message from a JSON or SSE
// (text/event-stream) response body. For SSE it takes the last "data:" frame.
// Both representations are valid for a streamable-http MCP server.
func parseJSONRPC(resp *http.Response) (*jsonRPCResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPBodyBytes))
	if err != nil {
		return nil, &mcpError{status: http.StatusBadGateway, msg: "failed to read the MCP server response"}
	}
	text := raw
	if strings.Contains(resp.Header.Get("Content-Type"), contentTypeEventStream) {
		data, ok := lastSSEDataFrame(raw)
		if !ok {
			return nil, &mcpError{
				status: http.StatusUnprocessableEntity,
				msg:    "the MCP server's SSE response contained no data frame",
			}
		}
		text = data
	}
	var msg jsonRPCResponse
	if err := json.Unmarshal(text, &msg); err != nil {
		return nil, &mcpError{
			status: http.StatusUnprocessableEntity,
			msg:    "the MCP server response was not valid JSON-RPC (is this an MCP endpoint?)",
		}
	}
	return &msg, nil
}

// lastSSEDataFrame returns the payload of the LAST "data:" line in an SSE body
// (an MCP streamable-http server frames the JSON-RPC message as one data line).
func lastSSEDataFrame(body []byte) ([]byte, bool) {
	var last []byte
	found := false
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			last = []byte(strings.TrimSpace(after))
			found = true
		}
	}
	return last, found
}
