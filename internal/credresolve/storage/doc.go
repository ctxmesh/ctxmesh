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

// Package storage builds the credential backend the token service is configured with (Kubernetes
// Secrets, Postgres or a remote provider) and migrates grants into it (ADR 0032, ADR 0152 §1).
//
// The backend is a configuration choice, not a rebuild. The token service builds exactly one at
// startup and every delegating sidecar shares it, so the cache and singleflight stay global
// (ADR 0030 §1). It is a subpackage so credresolve itself stays free of Kubernetes and database
// clients for the egress sidecar.
package storage
