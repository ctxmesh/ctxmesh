package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// The retry exists so a SLOW store does not become a dead process, and the ways it can go
// wrong are all silent: hanging forever, giving up instantly, or ignoring cancellation.
// Each of those is asserted here rather than assumed from reading the loop.

func TestWaitForStore_SucceedsOnceTheStoreComesUp(t *testing.T) {
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "10s")
	attempts := 0
	var retried int
	db, err := waitForStore(context.Background(),
		func(error, time.Duration) { retried++ },
		func(context.Context) (*sql.DB, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("connection refused")
			}
			return &sql.DB{}, nil
		})
	if err != nil {
		t.Fatalf("expected success once the store came up, got %v", err)
	}
	if db == nil {
		t.Fatal("expected a handle back")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if retried != 2 {
		t.Fatalf("expected 2 retry notifications, got %d", retried)
	}
}

func TestWaitForStore_GivesUpAtTheBudgetWithTheLastError(t *testing.T) {
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "1500ms")
	start := time.Now()
	_, err := waitForStore(context.Background(), nil,
		func(context.Context) (*sql.DB, error) { return nil, errors.New("connection refused") })
	if err == nil {
		t.Fatal("expected an error once the budget was spent")
	}
	// The operator has to be able to tell "slow" from "misconfigured", so the underlying
	// failure must survive rather than be replaced by a bare timeout.
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the last error was lost: %v", err)
	}
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("overran the 1.5s budget badly: %s", el)
	}
}

func TestWaitForStore_ZeroBudgetFailsFast(t *testing.T) {
	// The old behaviour must remain reachable: an operator who wants a crash rather than a
	// wait sets 0, and gets exactly one attempt.
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "0")
	attempts := 0
	_, err := waitForStore(context.Background(), nil,
		func(context.Context) (*sql.DB, error) {
			attempts++
			return nil, errors.New("connection refused")
		})
	if err == nil {
		t.Fatal("expected the error to propagate")
	}
	if attempts != 1 {
		t.Fatalf("zero budget must not retry; got %d attempts", attempts)
	}
}

func TestWaitForStore_HonoursContextCancellation(t *testing.T) {
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "5m")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := waitForStore(ctx, nil,
		func(context.Context) (*sql.DB, error) { return nil, errors.New("connection refused") })
	if err == nil {
		t.Fatal("expected cancellation to surface as an error")
	}
	// A cancelled context is a shutdown, not a slow dependency: it must not sit out the
	// remaining budget.
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("ignored cancellation for %s", el)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the last error was lost on cancellation: %v", err)
	}
}

func TestWaitForStore_RejectsAnUnparseableBudget(t *testing.T) {
	// Fail loudly rather than silently falling back to the default: a typo in a Deployment
	// env would otherwise change start-up behaviour with no signal.
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "5 minutes")
	_, err := waitForStore(context.Background(), nil,
		func(context.Context) (*sql.DB, error) { return &sql.DB{}, nil })
	if err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("expected a parse error naming the bad value, got %v", err)
	}
}

func TestNextBackoff_DoublesThenCapsAt15s(t *testing.T) {
	// An uncapped exponential is what turned a slow dependency into a five-minute sleep in
	// the first place. Asserted as a pure sequence: the previous version of this test slept
	// through the real backoff and took 60 seconds, which is how a suite becomes one people
	// skip.
	got := []time.Duration{}
	for d := firstBackoff; len(got) < 8; d = nextBackoff(d) {
		got = append(got, d)
	}
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		15 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}
