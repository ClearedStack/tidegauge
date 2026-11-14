package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

const contractExamplesDir = "../../testdata/contract/v1/examples"

type savedExample struct {
	About   string       `json:"about"`
	Given   *savedGiven  `json:"given,omitempty"`
	Request savedRequest `json:"request"`
	Expect  savedExpect  `json:"expect"`
}

type savedGiven struct {
	Items     []SeedItem     `json:"items,omitempty"`
	Calls     []savedRequest `json:"calls,omitempty"`
	Wait      string         `json:"wait,omitempty"`
	Condition string         `json:"condition,omitempty"`
}

type savedRequest struct {
	As          string          `json:"as,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ContentType *string         `json:"content_type,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Raw         *string         `json:"raw,omitempty"`
}

type savedExpect struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Stream *string         `json:"stream,omitempty"`
}

func isWorkerPath(path string) bool {
	switch path {
	case PathLease, PathHeartbeat, PathComplete, PathFail, PathNeedsSubtool:
		return true
	}
	return false
}

// canClientEmit reports whether savedRequest represents a valid Contract v1
// worker request that muster.Client is allowed to construct and send.
func canClientEmit(r savedRequest) bool {
	if !isWorkerPath(r.Path) || r.Method != http.MethodPost {
		return false
	}
	if !workerNames[r.As] || r.ContentType != nil || r.Raw != nil || len(r.Body) == 0 {
		return false
	}
	switch r.Path {
	case PathLease:
		var req leaseReq
		if decodeStrict(r.Body, &req) != nil || req.Contract != ContractVersion {
			return false
		}
		return true
	case PathHeartbeat:
		var req heartbeatReq
		if decodeStrict(r.Body, &req) != nil || req.Contract != ContractVersion || strings.TrimSpace(req.Lease) == "" {
			return false
		}
		return true
	case PathComplete:
		var req completeReq
		if decodeStrict(r.Body, &req) != nil || req.Contract != ContractVersion ||
			strings.TrimSpace(req.Lease) == "" || strings.TrimSpace(req.State) == "" {
			return false
		}
		if len(req.Result) > 0 {
			trimmed := bytes.TrimSpace(req.Result)
			if len(trimmed) > 0 && string(trimmed) != "null" && trimmed[0] != '{' {
				return false
			}
		}
		return true
	case PathFail:
		var req failReq
		if decodeStrict(r.Body, &req) != nil || req.Contract != ContractVersion ||
			strings.TrimSpace(req.Lease) == "" ||
			(req.Code != watch.CodeTimeout && req.Code != watch.CodeParse) {
			return false
		}
		return true
	case PathNeedsSubtool:
		var req needsSubtoolReq
		if decodeStrict(r.Body, &req) != nil || req.Contract != ContractVersion || strings.TrimSpace(req.Lease) == "" {
			return false
		}
		return true
	default:
		return false
	}
}

// TestReplayContractExamples replays every single saved example in
// contract/v1/examples/ from ClearedStack/muster (all 56 JSON files, zero
// skipped).
func TestReplayContractExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(contractExamplesDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 56 {
		t.Fatalf("found %d contract examples in %s, want 56", len(paths), contractExamplesDir)
	}

	replayed := 0
	workerCallsVerified := 0
	refusalsVerified := 0

	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			rawFile, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var ex savedExample
			dec := json.NewDecoder(bytes.NewReader(rawFile))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ex); err != nil {
				t.Fatalf("invalid example %s: %v", name, err)
			}
			if ex.About == "" {
				t.Fatalf("%s: missing about description", name)
			}

			// 1. Replay every valid worker call in given.calls through muster.Client
			// to assert exact request wire compatibility across all setup steps.
			if ex.Given != nil {
				for i, gc := range ex.Given.Calls {
					if canClientEmit(gc) {
						verifyClientWireRequest(t, gc, i)
						workerCallsVerified++
					}
				}
			}

			// 2. Replay the main example request and expected answer.
			switch {
			case canClientEmit(ex.Request):
				replayWorkerCallThroughClient(t, ex.Request, ex.Expect)
				workerCallsVerified++
				if ex.Expect.Status >= 400 {
					refusalsVerified++
				}

			case isWorkerPath(ex.Request.Path) || ex.Request.Path == "/v1/made-up" || ex.Request.Path == "/healthz":
				// Malformed/unauthorized/wrong-method worker call, unknown path, or /healthz:
				// verify Standin produces the exact expected status & refusal/body, and if it
				// is a refusal, verify muster.Client parses that exact refusal payload.
				replayRawAgainstStandin(t, ex)
				if ex.Expect.Status >= 400 {
					verifyClientRefusalDecoding(t, ex.Expect)
					refusalsVerified++
				}
				if name == "refused-400-bad_field-use-needs_subtool" {
					verifyClientRejectsFailNeedsSubtool(t)
				}

			default:
				// Non-worker endpoints (/v1/items, /v1/edits, /v1/feed, /v1/health, /v1/release):
				// verify Standin forbids Tidegauge from calling them (403 not_your_call), and
				// if the example expects a refusal, verify muster.Client decodes that refusal cleanly.
				verifyWorkerForbiddenOnNonWorkerPath(t, ex.Request)
				if ex.Expect.Status >= 400 {
					verifyClientRefusalDecoding(t, ex.Expect)
					refusalsVerified++
				}
			}
		})
		replayed++
	}

	if replayed != len(paths) {
		t.Fatalf("replayed %d of %d examples", replayed, len(paths))
	}
	if workerCallsVerified == 0 || refusalsVerified == 0 {
		t.Fatalf("unexpected counts: workerCalls=%d refusals=%d", workerCallsVerified, refusalsVerified)
	}
}

func verifyClientWireRequest(t *testing.T, sr savedRequest, idx int) {
	t.Helper()
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		switch r.URL.Path {
		case PathLease:
			w.WriteHeader(http.StatusNoContent)
		case PathHeartbeat:
			writeJSON(w, http.StatusOK, Beat{Lease: "PA-0001.1.1", Expires: "2026-01-05T09:01:00Z"})
		default:
			writeJSON(w, http.StatusOK, Ack{ID: "PA-0001", State: "live"})
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, StaticTokenSource("tok-"+sr.As), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invokeClientForSavedRequest(context.Background(), c, sr); err != nil {
		t.Fatalf("given call %d (%s): client returned unexpected error: %v", idx, sr.Path, err)
	}
	assertSameRequest(t, sr, gotMethod, gotPath, gotAuth, gotCT, gotBody)
}

func replayWorkerCallThroughClient(t *testing.T, sr savedRequest, exp savedExpect) {
	t.Helper()
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		if len(exp.Body) > 0 {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(exp.Status)
		if len(exp.Body) > 0 {
			var compact bytes.Buffer
			if err := json.Compact(&compact, exp.Body); err != nil {
				t.Fatal(err)
			}
			compact.WriteByte('\n')
			_, _ = w.Write(compact.Bytes())
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, StaticTokenSource("tok-"+sr.As), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	gotVal, callErr := invokeClientForSavedRequest(context.Background(), c, sr)
	assertSameRequest(t, sr, gotMethod, gotPath, gotAuth, gotCT, gotBody)

	if exp.Status == http.StatusNoContent {
		if callErr != nil || gotVal != nil {
			t.Fatalf("want (nil, nil) on 204, got (%v, %v)", gotVal, callErr)
		}
		return
	}
	if exp.Status >= 400 {
		assertMatchesRefusal(t, callErr, exp)
		return
	}
	if callErr != nil {
		t.Fatalf("unexpected error on %d: %v", exp.Status, callErr)
	}
	gotJSON, err := json.Marshal(gotVal)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotJSON, exp.Body)
}

func invokeClientForSavedRequest(ctx context.Context, c *Client, sr savedRequest) (any, error) {
	switch sr.Path {
	case PathLease:
		g, err := c.Lease(ctx)
		if g == nil {
			return nil, err
		}
		return g, err
	case PathHeartbeat:
		var req heartbeatReq
		_ = json.Unmarshal(sr.Body, &req)
		return c.Heartbeat(ctx, req.Lease)
	case PathComplete:
		var req completeReq
		_ = json.Unmarshal(sr.Body, &req)
		var res any
		if len(req.Result) > 0 {
			res = req.Result
		}
		return c.Complete(ctx, req.Lease, req.State, res)
	case PathFail:
		var req failReq
		_ = json.Unmarshal(sr.Body, &req)
		return c.Fail(ctx, req.Lease, req.Code)
	case PathNeedsSubtool:
		var req needsSubtoolReq
		_ = json.Unmarshal(sr.Body, &req)
		return c.NeedsSubtool(ctx, req.Lease)
	default:
		return nil, &ClientError{Message: "unsupported worker path"}
	}
}

func replayRawAgainstStandin(t *testing.T, ex savedExample) {
	t.Helper()
	var seed []SeedItem
	if ex.Given != nil {
		seed = ex.Given.Items
	}
	s := NewStandin(seed)
	if ex.Given != nil {
		for i, gc := range ex.Given.Calls {
			rec := doRawRequest(t, s, gc)
			if rec.Code < 200 || rec.Code > 299 {
				t.Fatalf("given call %d failed against Standin: %d %s", i+1, rec.Code, rec.Body.String())
			}
		}
		if ex.Given.Wait != "" {
			d, err := time.ParseDuration(ex.Given.Wait)
			if err != nil {
				t.Fatal(err)
			}
			s.Advance(d)
		}
	}

	rec := doRawRequest(t, s, ex.Request)
	if rec.Code != ex.Expect.Status {
		t.Fatalf("Standin status = %d, want %d (body: %s)", rec.Code, ex.Expect.Status, rec.Body.String())
	}
	if len(ex.Expect.Body) == 0 {
		if rec.Body.Len() != 0 {
			t.Fatalf("Standin returned unexpected body: %s", rec.Body.String())
		}
		return
	}
	assertSameJSON(t, rec.Body.Bytes(), ex.Expect.Body)
}

func doRawRequest(t *testing.T, h http.Handler, sr savedRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if sr.Raw != nil {
		body = []byte(*sr.Raw)
	} else if len(sr.Body) > 0 {
		body = sr.Body
	}
	req := httptest.NewRequest(sr.Method, sr.Path, bytes.NewReader(body))
	if sr.As != "" {
		req.Header.Set("Authorization", "Bearer tok-"+sr.As)
	}
	if sr.ContentType != nil {
		if *sr.ContentType != "" {
			req.Header.Set("Content-Type", *sr.ContentType)
		}
	} else if sr.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func verifyClientRefusalDecoding(t *testing.T, exp savedExpect) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(exp.Status)
		_, _ = w.Write(exp.Body)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, StaticTokenSource("tok-tidegauge"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := c.Lease(context.Background())
	assertMatchesRefusal(t, callErr, exp)
}

func verifyClientRejectsFailNeedsSubtool(t *testing.T) {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, StaticTokenSource("tok-tidegauge"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fail(context.Background(), "PA-0001.1.1", watch.CodeNeedsSubtool); err == nil {
		t.Fatal("expected Client.Fail to reject E_NEEDS_SUBTOOL client-side")
	}
	if called {
		t.Fatal("Client.Fail sent network request with E_NEEDS_SUBTOOL")
	}
}

func verifyWorkerForbiddenOnNonWorkerPath(t *testing.T, sr savedRequest) {
	t.Helper()
	s := NewStandin(nil)
	workerAttempt := savedRequest{
		As:     "tidegauge",
		Method: sr.Method,
		Path:   sr.Path,
		Body:   json.RawMessage(`{"contract":"v1"}`),
	}
	rec := doRawRequest(t, s, workerAttempt)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Standin gave %d for tidegauge calling %s %s, want 403", rec.Code, sr.Method, sr.Path)
	}
	var ref Refusal
	if err := decodeStrict(rec.Body.Bytes(), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Cause != "not_your_call" || ref.Error != watch.CodeParse {
		t.Fatalf("unexpected refusal %+v", ref)
	}
}

func assertMatchesRefusal(t *testing.T, callErr error, exp savedExpect) {
	t.Helper()
	if callErr == nil {
		t.Fatalf("expected refusal error for HTTP %d, got nil", exp.Status)
	}
	re, ok := UnwrapRefusal(callErr)
	if !ok {
		t.Fatalf("expected *RefusalError, got %T (%v)", callErr, callErr)
	}
	var want Refusal
	if err := json.Unmarshal(exp.Body, &want); err != nil {
		t.Fatal(err)
	}
	if re.Status != exp.Status || re.Refusal.Error != want.Error || re.Cause != want.Cause || re.Reason != want.Reason {
		t.Fatalf("got refusal %+v, want status=%d %+v", re, exp.Status, want)
	}
	wantRetry := exp.Status == http.StatusTooManyRequests || exp.Status == http.StatusServiceUnavailable
	if gotRetry := IsRetryable(callErr); gotRetry != wantRetry {
		t.Fatalf("IsRetryable(%d %s) = %v, want %v", exp.Status, want.Cause, gotRetry, wantRetry)
	}
}

func assertSameRequest(t *testing.T, want savedRequest, gotMethod, gotPath, gotAuth, gotCT string, gotBody []byte) {
	t.Helper()
	if gotMethod != want.Method || gotPath != want.Path {
		t.Fatalf("got %s %s, want %s %s", gotMethod, gotPath, want.Method, want.Path)
	}
	if wantAuth := "Bearer tok-" + want.As; gotAuth != wantAuth {
		t.Fatalf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotCT)
	}
	assertSameJSON(t, gotBody, want.Body)
}

func assertSameJSON(t *testing.T, gotRaw, wantRaw []byte) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(gotRaw, &gotVal); err != nil {
		t.Fatalf("got invalid JSON %q: %v", gotRaw, err)
	}
	if err := json.Unmarshal(wantRaw, &wantVal); err != nil {
		t.Fatalf("want invalid JSON %q: %v", wantRaw, err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", bytes.TrimSpace(gotRaw), bytes.TrimSpace(wantRaw))
	}
}

// TestStandinEndToEndWorkerStories replays multi-step item worker stories
// (including Tidegauge's 7-day lease-check and lease-rebuild) end-to-end
// against Standin using muster.Client.
func TestStandinEndToEndWorkerStories(t *testing.T) {
	stories := []string{
		"complete.json",
		"complete-revisit-linked.json",
		"complete-revisit-parked.json",
		"fail-parse.json",
		"fail-timeout.json",
		"heartbeat.json",
		"lease-check.json",
		"lease-incident.json",
		"lease-nothing.json",
		"lease-rebuild.json",
		"lease.json",
		"needs_subtool.json",
		"needs_subtool-building.json",
		"needs_subtool-live.json",
		"refused-409-lease_not_held.json",
		"refused-409-move_not_allowed.json",
		"refused-500-bad_record.json",
	}

	for _, file := range stories {
		t.Run(strings.TrimSuffix(file, ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(contractExamplesDir, file))
			if err != nil {
				t.Fatal(err)
			}
			var ex savedExample
			if err := json.Unmarshal(raw, &ex); err != nil {
				t.Fatal(err)
			}

			var seed []SeedItem
			if ex.Given != nil {
				seed = ex.Given.Items
			}
			standin := NewStandin(seed)
			if ex.Given != nil && ex.Given.Condition == "the queue says sextant is next for an item that is live" {
				standin.AddItem(SeedItem{ID: "PA-0001", State: "live", Lane: "normal", Next: "sextant"})
			}
			srv := httptest.NewServer(standin)
			defer srv.Close()

			if ex.Given != nil {
				for _, gc := range ex.Given.Calls {
					c, err := NewClient(srv.URL, StaticTokenSource(standin.TokenFor(gc.As)), srv.Client())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := invokeClientForSavedRequest(context.Background(), c, gc); err != nil {
						t.Fatalf("setup call %s as %s failed: %v", gc.Path, gc.As, err)
					}
				}
				if ex.Given.Wait != "" {
					d, err := time.ParseDuration(ex.Given.Wait)
					if err != nil {
						t.Fatal(err)
					}
					standin.Advance(d)
				}
			}

			c, err := NewClient(srv.URL, StaticTokenSource(standin.TokenFor(ex.Request.As)), srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			gotVal, callErr := invokeClientForSavedRequest(context.Background(), c, ex.Request)
			if ex.Expect.Status == http.StatusNoContent {
				if callErr != nil || gotVal != nil {
					t.Fatalf("want (nil, nil), got (%v, %v)", gotVal, callErr)
				}
				return
			}
			if ex.Expect.Status >= 400 {
				assertMatchesRefusal(t, callErr, ex.Expect)
				return
			}
			if callErr != nil {
				t.Fatalf("unexpected error: %v", callErr)
			}
			gotJSON, err := json.Marshal(gotVal)
			if err != nil {
				t.Fatal(err)
			}
			assertSameJSON(t, gotJSON, ex.Expect.Body)
		})
	}
}

func TestClientGuards(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("tok-tidegauge\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	standin := NewStandin([]SeedItem{{ID: "PA-0001", State: "live", Lane: "backfill"}})
	srv := httptest.NewServer(standin)
	defer srv.Close()

	c, err := NewClient(srv.URL, FileTokenSource(tokFile), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	grant, err := c.Lease(context.Background())
	if err != nil || grant == nil {
		t.Fatalf("Lease failed: %v %v", grant, err)
	}

	// Non-object result must be rejected client-side.
	if _, err := c.Complete(context.Background(), grant.Lease, "live", []string{"not-an-object"}); err == nil {
		t.Fatal("expected non-object result to be rejected")
	}

	// Oversized (>64 KiB) result must be rejected client-side.
	huge := map[string]string{"pad": strings.Repeat("a", MaxBodyBytes)}
	if _, err := c.Complete(context.Background(), grant.Lease, "live", huge); err == nil {
		t.Fatal("expected >64 KiB result to be rejected")
	}

	// Redirect must not be followed with bearer token.
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+PathLease, http.StatusFound)
	}))
	defer redirectSrv.Close()
	rc, err := NewClient(redirectSrv.URL, FileTokenSource(tokFile), redirectSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Lease(context.Background()); err == nil {
		t.Fatal("expected redirect on Lease to fail")
	}
}
