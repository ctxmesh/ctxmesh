package namespacetenant

import (
	"context"
	"testing"
)

// The defect this guards, stated as a test: a tenancy-less install must still enumerate namespaces.
//
// namespace_tenants is written only by the Tenant controller, and tenancy is opt-in — so on a stock
// install it is empty. AllNamespaces returning nothing there left the console's picker empty,
// workingNamespace "", and a CLUSTER-scoped capability probe that no namespaced RoleBinding can
// satisfy: every flow reported read-only to a user who genuinely held operator rights.
//
// A caller bound per-namespace is the SHIPPED default (ADR 0046 + catalog.go's isolation
// precondition), so "no tenants" is the ordinary case, not an edge one.
func TestAllNamespacesSeesDiscoveryOnlyNamespacesWithNoTenants(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	// No tenant has ever been reconciled — the stock-install state.
	before, err := s.AllNamespaces(ctx)
	if err != nil {
		t.Fatalf("AllNamespaces: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("expected an empty mirror before discovery, got %v", before)
	}

	if err := s.RecordNamespace(ctx, "team-b"); err != nil {
		t.Fatalf("RecordNamespace: %v", err)
	}
	if err := s.RecordNamespace(ctx, "team-a"); err != nil {
		t.Fatalf("RecordNamespace: %v", err)
	}

	got, err := s.AllNamespaces(ctx)
	if err != nil {
		t.Fatalf("AllNamespaces: %v", err)
	}
	if len(got) != 2 || got[0] != "team-a" || got[1] != "team-b" {
		t.Fatalf("a tenancy-less install must still enumerate its namespaces, sorted; got %v", got)
	}
}

// Discovery must not invent tenant attribution: it widens the candidate set only.
func TestRecordNamespaceGrantsNoTenantMembership(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	if err := s.RecordNamespace(ctx, "team-a"); err != nil {
		t.Fatalf("RecordNamespace: %v", err)
	}
	if _, ok, err := s.TenantOf(ctx, "team-a"); err != nil || ok {
		t.Fatalf("a discovered namespace must have no tenant; ok=%v err=%v", ok, err)
	}
}

// A tenant-attributed namespace is not un-attributed by forgetting its discovery row — the two
// sources are independent, and ForgetNamespace must never prune what the Tenant controller owns.
func TestForgetNamespaceLeavesTenantAttributionAlone(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	if err := s.SetMembers(ctx, "acme", []string{"team-a"}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	if err := s.RecordNamespace(ctx, "team-a"); err != nil {
		t.Fatalf("RecordNamespace: %v", err)
	}
	if err := s.ForgetNamespace(ctx, "team-a"); err != nil {
		t.Fatalf("ForgetNamespace: %v", err)
	}

	tenant, ok, err := s.TenantOf(ctx, "team-a")
	if err != nil || !ok || tenant != "acme" {
		t.Fatalf("tenant attribution must survive forgetting discovery; got %q ok=%v err=%v", tenant, ok, err)
	}
	got, err := s.AllNamespaces(ctx)
	if err != nil || len(got) != 1 || got[0] != "team-a" {
		t.Fatalf("a tenant member must still enumerate; got %v err=%v", got, err)
	}
}
