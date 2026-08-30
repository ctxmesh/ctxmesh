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

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const controllerSA = "system:serviceaccount:agentry:agentry-controller-manager"

func testValidator(t *testing.T) *TenantLabelValidator {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return NewTenantLabelValidator(admission.NewDecoder(s), controllerSA)
}

func ns(labels map[string]string) []byte {
	raw, _ := json.Marshal(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-x", Labels: labels},
	})
	return raw
}

func req(user string, newLabels, oldLabels map[string]string) admission.Request {
	r := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UserInfo: authenticationv1.UserInfo{Username: user},
		Object:   runtime.RawExtension{Raw: ns(newLabels)},
	}}
	if oldLabels != nil {
		r.OldObject = runtime.RawExtension{Raw: ns(oldLabels)}
	}
	return r
}

func TestTenantLabelValidator(t *testing.T) {
	v := testValidator(t)
	tenant := map[string]string{tenantLabel: "victim-tenant"}
	other := map[string]string{tenantLabel: "my-own-tenant"}
	none := map[string]string{"unrelated": "x"}

	cases := []struct {
		name    string
		req     admission.Request
		allowed bool
	}{
		{"controller may set the label", req(controllerSA, tenant, nil), true},
		{"attacker CREATE with the tenant label is denied", req("someuser", tenant, nil), false},
		{"attacker UPDATE setting the label (old had none) is denied", req("someuser", tenant, none), false},
		{"attacker UPDATE changing the label is denied", req("someuser", tenant, other), false},
		{"attacker UPDATE with an unchanged label is allowed (no-op)", req("someuser", tenant, tenant), true},
		{"attacker REMOVING the label is DENIED (ADR 0102: removal detaches quota scoping — controller-mediated only)", req("someuser", none, tenant), false},
		{"attacker CREATE without the label is allowed", req("someuser", none, nil), true},
		{"attacker UPDATE that never touches the label is allowed", req("someuser", none, map[string]string{"unrelated": "y"}), true},
		{"the controller may REMOVE the label (reassignment)", req(controllerSA, none, tenant), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := v.Handle(context.Background(), tc.req)
			assert.Equal(t, tc.allowed, resp.Allowed, "denial message: %s", denialMsg(resp))
		})
	}
}

func denialMsg(r admission.Response) string {
	if r.Result != nil {
		return r.Result.Message
	}
	return ""
}
