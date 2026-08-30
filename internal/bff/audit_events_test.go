package bff

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
)

// errStore is an audit store whose Append always fails — proves appendAudit is best-effort.
type errStore struct{}

func (errStore) Append(context.Context, auditlog.Entry) error { return errors.New("db down") }
func (errStore) List(context.Context, auditlog.Query) (auditlog.Page, error) {
	return auditlog.Page{}, nil
}
func (errStore) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func TestAppendAudit_LandsAsABFFEventWithTheActor(t *testing.T) {
	store := auditlog.NewMemStore()
	s := &Server{auditStore: store, log: logr.Discard()}

	s.appendAudit(context.Background(), auditlog.Entry{
		Actor: "alice", Action: auditActionGrantCreate,
		ResourceKind: "MCPGrant", ResourceName: "scalekit", Namespace: "ns1",
	})

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	got := page.Items[0]
	assert.Equal(t, "bff", got.Source, "the BFF stamps source=bff")
	assert.Equal(t, "alice", got.Actor, "the precise actor is preserved")
	assert.Equal(t, "user", got.ActorKind, "actor_kind defaults to user for BFF events")
	assert.Equal(t, auditActionGrantCreate, got.Action)
	assert.Equal(t, "MCPGrant", got.ResourceKind)
	assert.Equal(t, "success", got.Outcome, "outcome defaults to success")
}

func TestAppendAudit_DeniedOutcomeIsPreserved(t *testing.T) {
	store := auditlog.NewMemStore()
	s := &Server{auditStore: store, log: logr.Discard()}
	s.appendAudit(context.Background(), auditlog.Entry{
		Actor: "bob", Action: auditActionConnect, ResourceKind: "Provider",
		ResourceName: "anthropic", Namespace: "ns1", Outcome: "denied",
	})
	page, _ := store.List(context.Background(), auditlog.Query{})
	require.Len(t, page.Items, 1)
	assert.Equal(t, "denied", page.Items[0].Outcome, "a denied action is recorded as such")
}

func TestAppendAudit_NilStoreIsNoop(t *testing.T) {
	s := &Server{auditStore: nil, log: logr.Discard()}
	// Must not panic — audit unwired ⇒ the BFF just doesn't persist (the controller still does).
	s.appendAudit(context.Background(), auditlog.Entry{Actor: "x", Action: auditActionConnect})
}

func TestAppendAudit_StoreErrorNeverFailsTheAction(t *testing.T) {
	s := &Server{auditStore: errStore{}, log: logr.Discard()}
	// appendAudit returns nothing; a failing store logs but the caller's action proceeds. The test
	// asserts it returns normally (no panic) — the "observability, never a gate" contract.
	s.appendAudit(context.Background(), auditlog.Entry{Actor: "x", Action: auditActionConnect})
}

func TestAuditActor_NilCallerIsUnknown(t *testing.T) {
	s := &Server{log: logr.Discard()}
	assert.Equal(t, "unknown", s.auditActor(context.Background(), nil))
}
