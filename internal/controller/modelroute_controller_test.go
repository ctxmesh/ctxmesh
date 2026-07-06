//go:build integration

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

package controller

// ModelRoute integration tests live here. Full test coverage (render loop,
// config-hash rollout, SecretBinding resolution, Ready condition lifecycle)
// is implemented in m2.3 alongside the reconciler body.
//
// This file is intentionally empty in m2.2 — the reconciler stub compiles and
// the CRDs install, but envtest-backed tests require the full reconcile logic.
// See internal/controller/suite_test.go for the TestMain bootstrap shared
// across all integration tests in this package.
