package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/tidegauge/internal/fetch"
	"github.com/ClearedStack/tidegauge/internal/muster"
	"github.com/ClearedStack/tidegauge/internal/spool"
)

func TestWorkerLiveLifecycleAndRetire(t *testing.T) {
	var version atomic.Int32
	version.Store(1)

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ecfr/part-901.xml":
			v := version.Load()
			switch v {
			case 1:
				if r.Header.Get("If-None-Match") == `"ecfr-v1"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("ETag", `"ecfr-v1"`)
				_, _ = w.Write([]byte(`<DIV5 N="901" TYPE="PART"><HEAD>PART 901 - SYNTHETIC RULES</HEAD><DIV8 N="901.1" TYPE="SECTION"><HEAD>901.1 Scope.</HEAD><P>Threshold is $10,000.</P></DIV8></DIV5>`))
			case 2:
				w.Header().Set("ETag", `"ecfr-v2"`)
				_, _ = w.Write([]byte(`<DIV5 N="901" TYPE="PART"><HEAD>PART 901 - SYNTHETIC RULES</HEAD><DIV8 N="901.1" TYPE="SECTION"><HEAD>901.1 Scope.</HEAD><P>Threshold is $15,000.</P></DIV8></DIV5>`))
			case 3:
				w.Header().Set("ETag", `"ecfr-v3"`)
				_, _ = w.Write([]byte(`<DIV5 N="901" TYPE="PART"><HEAD>PART 901 [RESERVED]</HEAD></DIV5>`))
			}
		case "/tools/synth-calc/manifest.json":
			_, _ = w.Write([]byte(`{"slug":"synth-calc","status":"live","healthy":true,"rule_version":"99-CFR-901@2031-01-01"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer feedSrv.Close()

	chartroomRes, _ := json.Marshal(map[string]any{
		"slug":         "synth-calc",
		"status":       "live",
		"live_url":     feedSrv.URL + "/tools/synth-calc/manifest.json",
		"rule_version": "99-CFR-901@2031-01-01",
		"provenance": map[string]any{
			"links": []string{feedSrv.URL + "/ecfr/part-901.xml"},
		},
	})

	standin := muster.NewStandin([]muster.SeedItem{
		{
			ID:    "PA-0001",
			State: "live",
			Lane:  "backfill",
			Results: map[string]json.RawMessage{
				"chartroom": chartroomRes,
			},
		},
	})
	musterSrv := httptest.NewServer(standin)
	defer musterSrv.Close()

	client, err := muster.NewClient(musterSrv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), musterSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fetcher := fetch.New(fetch.Options{
		ScratchDir:          sp.TempDir(),
		HTTPClient:          feedSrv.Client(),
		AllowPrivateForTest: true,
	})
	w := New(Config{
		Muster:            client,
		Spool:             sp,
		Fetcher:           fetcher,
		HeartbeatInterval: 50 * time.Millisecond,
	})

	// 1. First check records baseline and completes live -> live (+7d delay).
	worked, err := w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("first StepOnce: worked=%v err=%v", worked, err)
	}
	st, results, _ := standin.InspectItem("PA-0001")
	if st != "live" {
		t.Fatalf("after check 1: state = %q, want live", st)
	}
	if bytes.Contains(results["tidegauge"], []byte("127.0.0.1")) {
		t.Fatalf("results.tidegauge leaked URL/IP: %s", results["tidegauge"])
	}

	// Immediately polling before 7 days passes yields 204 (worked=false).
	worked, err = w.StepOnce(context.Background())
	if err != nil || worked {
		t.Fatalf("expected idle before 168h, got worked=%v err=%v", worked, err)
	}

	// 2. Advance 7 days; ETag 304 Not Modified keeps item live -> live.
	standin.Advance(168 * time.Hour)
	worked, err = w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("second StepOnce: worked=%v err=%v", worked, err)
	}
	st, _, _ = standin.InspectItem("PA-0001")
	if st != "live" {
		t.Fatalf("after unchanged check 2: state = %q, want live", st)
	}

	// 3. Advance 7 days and bump eCFR threshold ($10,000 -> $15,000): live -> building!
	standin.Advance(168 * time.Hour)
	version.Store(2)
	worked, err = w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("third StepOnce: worked=%v err=%v", worked, err)
	}
	st, _, _ = standin.InspectItem("PA-0001")
	if st != "building" {
		t.Fatalf("after eCFR drift: state = %q, want building", st)
	}

	// 4. Test live -> retired when eCFR part becomes [Reserved].
	version.Store(3)
	standin.AddItem(muster.SeedItem{
		ID:    "PA-0002",
		State: "live",
		Lane:  "backfill",
		Results: map[string]json.RawMessage{
			"chartroom": chartroomRes,
		},
	})
	worked, err = w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("retire StepOnce: worked=%v err=%v", worked, err)
	}
	st2, res2, _ := standin.InspectItem("PA-0002")
	if st2 != "retired" {
		t.Fatalf("after repeal: state = %q, want retired", st2)
	}
	var sr StoredResult
	if err := json.Unmarshal(res2["tidegauge"], &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Retire == nil || sr.Retire.Reason != "repealed" || sr.Retire.Note == "" {
		t.Fatalf("missing Chartroom retire block: %+v", sr.Retire)
	}
}

func TestWorkerLinkedParkedErrorsAndHeartbeats(t *testing.T) {
	var slow atomic.Bool
	var linkedMode atomic.Int32 // 0=free, 1=paywall
	var parkedVer atomic.Int32  // 0=proposed, 1=final

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			time.Sleep(80 * time.Millisecond)
		}
		switch r.URL.Path {
		case "/linked":
			if linkedMode.Load() == 1 {
				w.WriteHeader(http.StatusPaymentRequired)
				return
			}
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><main><form><input name="x"></form></main></body></html>`))
		case "/federal-register/blocker.json":
			if parkedVer.Load() == 0 {
				_, _ = w.Write([]byte(`{"document_number":"2031-99001","type":"Proposed Rule","title":"Synthetic Proposed Rule","publication_date":"2031-01-10"}`))
			} else {
				_, _ = w.Write([]byte(`{"document_number":"2031-99001","type":"Rule","action":"Final rule.","title":"Synthetic Final Rule","publication_date":"2031-06-01","effective_on":"2031-07-01"}`))
			}
		case "/corrupt.xml":
			_, _ = w.Write([]byte(`<DIV5 N="901"`))
		case "/unfamiliar.xml":
			_, _ = w.Write([]byte(`<mystery><item/></mystery>`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer upstream.Close()

	standin := muster.NewStandin(nil)

	var heartbeats atomic.Int32
	musterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == muster.PathHeartbeat {
			heartbeats.Add(1)
		}
		standin.ServeHTTP(w, r)
	}))
	defer musterSrv.Close()

	client, err := muster.NewClient(musterSrv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), musterSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{
		Muster:  client,
		Spool:   sp,
		Fetcher: fetch.New(fetch.Options{ScratchDir: sp.TempDir(), HTTPClient: upstream.Client(), AllowPrivateForTest: true}),
		// Short heartbeat interval to verify active lease heartbeating during slow fetch.
		HeartbeatInterval: 20 * time.Millisecond,
	})

	// 1. Linked item adds a paywall -> observed, while emitting heartbeats during slow fetch.
	slow.Store(true)
	linkedMode.Store(1)
	linkedCR, _ := json.Marshal(map[string]any{
		"slug": "synth-ext",
		"link": map[string]any{"url": upstream.URL + "/linked"},
	})
	standin.AddItem(muster.SeedItem{
		ID:      "PA-0010",
		State:   "linked",
		Lane:    "backfill",
		Next:    "tidegauge",
		Results: map[string]json.RawMessage{"chartroom": linkedCR},
	})
	if worked, err := w.StepOnce(context.Background()); err != nil || !worked {
		t.Fatalf("linked StepOnce: %v %v", worked, err)
	}
	slow.Store(false)
	if heartbeats.Load() == 0 {
		t.Fatal("expected at least one heartbeat during slow upstream check")
	}
	if st, _, _ := standin.InspectItem("PA-0010"); st != "observed" {
		t.Fatalf("linked state = %q, want observed", st)
	}

	// 2. Parked item: baseline recorded (parked -> parked), then blocker finalizes -> observed.
	csRes, _ := json.Marshal(map[string]any{"blocker_url": upstream.URL + "/federal-register/blocker.json"})
	standin.AddItem(muster.SeedItem{
		ID:      "PA-0011",
		State:   "parked",
		Lane:    "backfill",
		Next:    "tidegauge",
		Results: map[string]json.RawMessage{"countersign": csRes},
	})
	if _, err := w.StepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := standin.InspectItem("PA-0011"); st != "parked" {
		t.Fatalf("parked initial state = %q, want parked", st)
	}
	standin.Advance(168 * time.Hour)
	parkedVer.Store(1)
	if _, err := w.StepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := standin.InspectItem("PA-0011"); st != "observed" {
		t.Fatalf("parked after blocker change = %q, want observed", st)
	}

	// 3. Corrupt XML -> fail(E_PARSE), item held in live and Bellbuoy incident created.
	badRes, _ := json.Marshal(map[string]any{
		"targets": []map[string]string{{"tool": "ecfr", "url": upstream.URL + "/corrupt.xml"}},
	})
	standin.AddItem(muster.SeedItem{
		ID:      "PA-0012",
		State:   "live",
		Lane:    "backfill",
		Results: map[string]json.RawMessage{"waybill": badRes},
	})
	if _, err := w.StepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := standin.InspectItem("PA-0012"); st != "live" {
		t.Fatalf("PA-0012 state = %q, want held in live", st)
	}

	// 4. Unfamiliar XML -> needs_subtool.
	unfamRes, _ := json.Marshal(map[string]any{
		"targets": []map[string]string{{"tool": "ecfr", "url": upstream.URL + "/unfamiliar.xml"}},
	})
	standin.AddItem(muster.SeedItem{
		ID:      "PA-0013",
		State:   "live",
		Lane:    "backfill",
		Results: map[string]json.RawMessage{"waybill": unfamRes},
	})
	if _, err := w.StepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := standin.InspectItem("PA-0013"); st != "needs_subtool" {
		t.Fatalf("PA-0013 state = %q, want needs_subtool", st)
	}
}

func TestWorkerWaybillStructuredSources(t *testing.T) {
	var threshold atomic.Int32
	threshold.Store(10000)

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ecfr/part-31.xml":
			if threshold.Load() == 10000 {
				_, _ = w.Write([]byte(`<DIV5 N="31" TYPE="PART"><HEAD>PART 31 - CONTRACT COST PRINCIPLES</HEAD><DIV8 N="31.205" TYPE="SECTION"><HEAD>31.205 Selected costs.</HEAD><P>Cap is $10,000.</P></DIV8></DIV5>`))
			} else {
				_, _ = w.Write([]byte(`<DIV5 N="31" TYPE="PART"><HEAD>PART 31 - CONTRACT COST PRINCIPLES</HEAD><DIV8 N="31.205" TYPE="SECTION"><HEAD>31.205 Selected costs.</HEAD><P>Cap is $25,000.</P></DIV8></DIV5>`))
			}
		case "/tools/cost-cap/manifest.json":
			_, _ = w.Write([]byte(`{"slug":"cost-cap","status":"live","healthy":true,"rule_version":"48-CFR-31.205@2031-02-01"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer feedSrv.Close()

	// Exact Waybill unified results.waybill shape with structured WatchedSource objects
	// plus human-facing provenance links and Chartroom live_url without rule_version.
	waybillRes, _ := json.Marshal(map[string]any{
		"slug":         "cost-cap",
		"rule_version": "48-CFR-31.205@2031-02-01",
		"entry": map[string]any{
			"title":   "Cost Cap Helper",
			"summary": "Computes synthetic FAR 31.205 cost caps.",
		},
		"provenance": map[string]any{
			"note":  "Built from 48 CFR 31.205.",
			"links": []string{"https://www.acquisition.gov/far/part-31"},
		},
		"sources": []map[string]string{
			{
				"watcher":  "ecfr",
				"url":      feedSrv.URL + "/ecfr/part-31.xml",
				"citation": "48 CFR 31.205",
			},
		},
	})
	chartroomRes, _ := json.Marshal(map[string]any{
		"live_url": feedSrv.URL + "/tools/cost-cap/manifest.json",
	})

	standin := muster.NewStandin([]muster.SeedItem{
		{
			ID:    "PA-0042",
			State: "live",
			Lane:  "backfill",
			Results: map[string]json.RawMessage{
				"waybill":   waybillRes,
				"chartroom": chartroomRes,
			},
		},
	})
	musterSrv := httptest.NewServer(standin)
	defer musterSrv.Close()

	client, err := muster.NewClient(musterSrv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), musterSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{
		Muster:  client,
		Spool:   sp,
		Fetcher: fetch.New(fetch.Options{ScratchDir: sp.TempDir(), HTTPClient: feedSrv.Client(), AllowPrivateForTest: true}),
	})

	// 1. Initial baseline succeeds and stays live (verifies slug + rule_version came from results.waybill).
	if worked, err := w.StepOnce(context.Background()); err != nil || !worked {
		t.Fatalf("initial StepOnce: worked=%v err=%v", worked, err)
	}
	if st, _, _ := standin.InspectItem("PA-0042"); st != "live" {
		t.Fatalf("after baseline: state = %q, want live", st)
	}

	// 2. After 7 days and eCFR threshold drift, item moves live -> building.
	standin.Advance(168 * time.Hour)
	threshold.Store(25000)
	if worked, err := w.StepOnce(context.Background()); err != nil || !worked {
		t.Fatalf("drift StepOnce: worked=%v err=%v", worked, err)
	}
	if st, _, _ := standin.InspectItem("PA-0042"); st != "building" {
		t.Fatalf("after structured Waybill source drift: state = %q, want building", st)
	}
}
