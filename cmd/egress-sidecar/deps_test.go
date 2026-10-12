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

import (
	"os/exec"
	"strings"
	"testing"
)

// The sidecar runs in every tool-bound agent pod and reaches credentials only through the token
// service. A Kubernetes client in its import graph means it can talk to the API server again, with
// whatever identity the agent pod carries, and adds hundreds of packages to an image in every pod.
func TestEgressSidecarImportsNoKubernetesPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var k8s []string
	for pkg := range strings.FieldsSeq(string(out)) {
		if strings.HasPrefix(pkg, "k8s.io/") || strings.HasPrefix(pkg, "sigs.k8s.io/") {
			k8s = append(k8s, pkg)
		}
	}
	if len(k8s) > 0 {
		t.Fatalf("the egress sidecar depends on %d Kubernetes packages, e.g. %v", len(k8s), k8s[:min(3, len(k8s))])
	}
}
