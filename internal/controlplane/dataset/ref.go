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

package dataset

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// parseRef splits a datasetRef into (name, version, pinned). "name@version" → (name, version, true) — an
// immutable pin. A bare "name" → (name, 0, false) — the caller resolves the latest pinned version. Shared by
// both store implementations so the pg store and the twin agree on the ref grammar (ADR 0062 Fork 1). A malformed
// ref (blank name, blank/non-numeric/non-positive version, more than one '@') is controlplane.ErrInvalid.
func parseRef(ref string) (name string, version int, pinned bool, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, false, fmt.Errorf("dataset: %w: empty datasetRef", controlplane.ErrInvalid)
	}
	name, verStr, hasAt := strings.Cut(ref, "@")
	if !hasAt {
		return ref, 0, false, nil
	}
	if name == "" {
		return "", 0, false, fmt.Errorf("dataset: %w: datasetRef %q has an empty name", controlplane.ErrInvalid, ref)
	}
	if strings.ContainsRune(verStr, '@') {
		return "", 0, false, fmt.Errorf("dataset: %w: datasetRef %q has more than one '@'", controlplane.ErrInvalid, ref)
	}
	v, convErr := strconv.Atoi(verStr)
	if convErr != nil || v <= 0 {
		return "", 0, false, fmt.Errorf("dataset: %w: datasetRef %q has a non-positive-integer version", controlplane.ErrInvalid, ref)
	}
	return name, v, true, nil
}
