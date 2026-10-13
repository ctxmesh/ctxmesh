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

// Package credplane is the central token service's API: its HTTP server, wire protocol, client,
// caller authentication and TLS configuration (ADR 0030 §1, ADR 0154).
//
// One central Deployment in the locked credential namespace runs the credresolve backend behind
// this API; the per-pod egress sidecars DELEGATE cache-miss resolution to it instead of each
// embedding the backend. This consolidates the two operations that hit ceilings — grant-Secret
// reads and OAuth refresh — behind ONE global singleflight, so a busy multi-user fleet does
// N-sidecar work as one refresh, and the rotating-refresh-token race across pods (ADR 0029 §6 R3)
// disappears.
//
// The wire protocol is a tiny JSON request/response, over TLS when the service has a serving
// certificate. Every caller authenticates with a projected ServiceAccount token and the service
// derives who it acts for from that token and the forwarded run capability (ADR 0154). Long-term
// memory (controlplane/agentmemory/recall) and knowledge search (controlplane/knowledge/retrieval)
// are served behind the same authentication; this package owns only their routes, scope checks and
// response envelopes. The Client implements credresolve.CredentialResolver; it is the egress
// sidecar's only resolver.
package credplane
