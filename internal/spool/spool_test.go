package spool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/muster"
	"github.com/ClearedStack/tidegauge/internal/watch"
)

func TestSpoolSurvivesMusterRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	standin := muster.NewStandin([]muster.SeedItem{
		{ID: "PA-0001", State: "live", Lane: "backfill"},
	})
	var down atomic.Bool
	down.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() && r.URL.Path != muster.PathLease {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"E_TIMEOUT","cause":"store_slow","reason":"the record store took too long"}`))
			return
		}
		standin.ServeHTTP(w, r)
	}))
	defer srv.Close()

	client, err := muster.NewClient(srv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	grant, err := client.Lease(context.Background())
	if err != nil || grant == nil {
		t.Fatalf("Lease failed: %v %v", grant, err)
	}

	if _, err := q.Enqueue(Entry{
		ItemID: grant.ID,
		Lease:  grant.Lease,
		Call:   CallComplete,
		State:  "building",
		Result: json.RawMessage(`{"status":"changed"}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Muster is returning 503 store_slow: Flush must keep the entry on disk.
	if err := q.Flush(context.Background(), client, nil); err == nil {
		t.Fatal("expected retryable error while Muster is unavailable")
	}
	if n, _ := q.Pending(); n != 1 {
		t.Fatalf("Pending = %d, want 1", n)
	}

	// Simulate Tidegauge process restart reopening the same spool directory,
	// and Muster coming back online.
	q2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	down.Store(false)
	if err := q2.Flush(context.Background(), client, nil); err != nil {
		t.Fatalf("Flush after recovery failed: %v", err)
	}
	if n, _ := q2.Pending(); n != 0 {
		t.Fatalf("Pending after Flush = %d, want 0", n)
	}

	state, _, ok := standin.InspectItem("PA-0001")
	if !ok || state != "building" {
		t.Fatalf("item state = %q (ok=%v), want building", state, ok)
	}
}

func TestSpoolDropsTerminalRefusalCleanly(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	standin := muster.NewStandin([]muster.SeedItem{
		{ID: "PA-0002", State: "live", Lane: "backfill"},
	})
	srv := httptest.NewServer(standin)
	defer srv.Close()

	client, err := muster.NewClient(srv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := client.Lease(context.Background())
	if err != nil || grant == nil {
		t.Fatalf("Lease failed: %v %v", grant, err)
	}

	// Attempting a disallowed move (live -> rejected) is refused with
	// 409 move_not_allowed. Spool must drop it cleanly rather than wedge.
	if _, err := q.Enqueue(Entry{
		ItemID: grant.ID,
		Lease:  grant.Lease,
		Call:   CallComplete,
		State:  "rejected",
	}); err != nil {
		t.Fatal(err)
	}

	var logged string
	if err := q.Flush(context.Background(), client, func(format string, args ...any) {
		logged = format
	}); err != nil {
		t.Fatalf("Flush returned error on terminal refusal: %v", err)
	}
	if logged == "" {
		t.Fatal("expected sanitized log message when dropping refused entry")
	}
	if n, _ := q.Pending(); n != 0 {
		t.Fatalf("Pending = %d, want 0", n)
	}

	// Also verify E_TIMEOUT via CallFail succeeds and clears spool.
	if _, err := q.Enqueue(Entry{
		ItemID: grant.ID,
		Lease:  grant.Lease,
		Call:   CallFail,
		Code:   watch.CodeTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.Flush(context.Background(), client, nil); err != nil {
		t.Fatalf("Flush(CallFail) failed: %v", err)
	}
	if n, _ := q.Pending(); n != 0 {
		t.Fatalf("Pending = %d, want 0", n)
	}
}
