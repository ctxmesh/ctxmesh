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

package enduseroidc

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeniedIP is the SSRF guard's IP-classification contract (ADR 0106 §8): the cloud-metadata address
// and link-local/loopback/multicast are ALWAYS denied; private (RFC1918/ULA) is denied unless opted in;
// public is always allowed.
func TestDeniedIP(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		allowLB      bool
		wantDenied   bool
		note         string
	}{
		{"169.254.169.254", true, true, true, "cloud metadata is ALWAYS denied (link-local), even with both opt-ins"},
		{"169.254.10.1", false, false, true, "link-local denied"},
		{"127.0.0.1", false, false, true, "loopback denied by default"},
		{"127.0.0.1", false, true, false, "loopback allowed with AllowLoopback (dev)"},
		{"::1", false, false, true, "IPv6 loopback denied"},
		{"0.0.0.0", false, false, true, "unspecified denied"},
		{"224.0.0.1", false, false, true, "multicast denied"},
		{"10.0.0.5", false, false, true, "RFC1918 private denied by default"},
		{"10.0.0.5", true, false, false, "RFC1918 private allowed with AllowPrivateIssuer (in-cluster IdP)"},
		{"172.16.5.5", false, false, true, "RFC1918 172.16/12 private denied"},
		{"192.168.1.1", false, false, true, "RFC1918 192.168/16 private denied"},
		{"fd00::1", false, false, true, "IPv6 ULA private denied by default"},
		{"fd00:ec2::254", true, true, false, "IPv6 ULA allowed with AllowPrivateIssuer"},
		{"8.8.8.8", false, false, false, "public allowed"},
		{"93.184.216.34", false, false, false, "public allowed"},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		got := deniedIP(ip, tc.allowPrivate, tc.allowLB) != ""
		assert.Equal(t, tc.wantDenied, got, "%s (%s)", tc.ip, tc.note)
	}
}
