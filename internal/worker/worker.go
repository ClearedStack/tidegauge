// Package worker runs Tidegauge's continuous Muster polling loop, executing
// deterministic sub-tools against leased items, heartbeating active leases and
// staging every outcome in /var/spool/cs before reporting back to Muster.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ClearedStack/tidegauge/internal/fetch"
	"github.com/ClearedStack/tidegauge/internal/muster"
	"github.com/ClearedStack/tidegauge/internal/spool"
	"github.com/ClearedStack/tidegauge/internal/watch"
	"github.com/ClearedStack/tidegauge/tools/ecfr"
	federalregister "github.com/ClearedStack/tidegauge/tools/federal-register"
	linkedtoolwatch "github.com/ClearedStack/tidegauge/tools/linked-tool-watch"
	livecatalog "github.com/ClearedStack/tidegauge/tools/live-catalog"
	nistcsrc "github.com/ClearedStack/tidegauge/tools/nist-csrc"
)

const (
	defaultPollInterval      = 5 * time.Second
	defaultRetryBackoff      = 2 * time.Second
	defaultHeartbeatInterval = 20 * time.Second
)

// Config configures a Tidegauge Worker.
type Config struct {
	Muster            *muster.Client
	Spool             *spool.Queue
	Fetcher           *fetch.Fetcher
	PollInterval      time.Duration
	RetryBackoff      time.Duration
	HeartbeatInterval time.Duration
	Logf              func(format string, args ...any)
}

// Worker coordinates Muster leases, deterministic sub-tool checks and spool delivery.
type Worker struct {
	muster            *muster.Client
	spool             *spool.Queue
	fetcher           *fetch.Fetcher
	pollInterval      time.Duration
	retryBackoff      time.Duration
	heartbeatInterval time.Duration
	logf              func(format string, args ...any)
}

// New constructs a Worker with safe defaults.
func New(cfg Config) *Worker {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	hb := cfg.HeartbeatInterval
	if hb <= 0 {
		hb = defaultHeartbeatInterval
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	fetcher := cfg.Fetcher
	if fetcher == nil {
		scratch := ""
		if cfg.Spool != nil {
			scratch = cfg.Spool.TempDir()
		}
		fetcher = fetch.New(fetch.Options{ScratchDir: scratch})
	}
	return &Worker{
		muster:            cfg.Muster,
		spool:             cfg.Spool,
		fetcher:           fetcher,
		pollInterval:      poll,
		retryBackoff:      backoff,
		heartbeatInterval: hb,
		logf:              logf,
	}
}

// StoredResult is persisted as results.tidegauge in Muster/Logbook (<64 KiB)
// so future 7-day checks can compare deterministic fingerprints and ETags
// without storing any source prose, HTML or raw URLs.
type StoredResult struct {
	Status       string        `json:"status"`
	ToolsChecked []string      `json:"tools_checked,omitempty"`
	DriftTools   []string      `json:"drift_tools,omitempty"`
	Signals      []string      `json:"signals,omitempty"`
	Baselines    []TargetState `json:"baselines,omitempty"`
	Retire       *RetireBlock  `json:"retire,omitempty"`
}

// RetireBlock matches Chartroom's retirement contract when Tidegauge moves a
// live item to retired ("reason": "repealed" | "superseded", "note": "...").
type RetireBlock struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// TargetState holds the compact baseline for one watched target keyed by an
// irreversible hash of (tool, url).
type TargetState struct {
	ID          string                   `json:"id"`
	Tool        string                   `json:"tool"`
	ETag        string                   `json:"etag,omitempty"`
	Digest      string                   `json:"digest"`
	Repealed    bool                     `json:"repealed,omitempty"`
	ECFR        *ecfr.Summary            `json:"ecfr,omitempty"`
	FedReg      *federalregister.Summary `json:"federal_register,omitempty"`
	NIST        *nistcsrc.Summary        `json:"nist_csrc,omitempty"`
	LinkedTool  *linkedtoolwatch.Summary `json:"linked_tool,omitempty"`
	LiveCatalog *livecatalog.Summary     `json:"live_catalog,omitempty"`
}

type watchTarget struct {
	id           string
	tool         string
	url          string
	expectedSlug string
	expectedRule string
}

// Run loops until ctx is cancelled, draining any spooled outcomes and polling
// Muster for work.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := w.StepOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logf("tidegauge: transient error in worker cycle, backing off")
			if !sleepCtx(ctx, w.retryBackoff) {
				return nil
			}
			continue
		}
		if !worked {
			if !sleepCtx(ctx, w.pollInterval) {
				return nil
			}
		}
	}
}

// StepOnce flushes any pending spooled reports and, if the outbox is clear,
// leases and processes at most one item from Muster. It returns worked=true
// when a lease was granted and processed.
func (w *Worker) StepOnce(ctx context.Context) (bool, error) {
	if err := w.spool.Flush(ctx, w.muster, w.logf); err != nil {
		return false, err
	}

	grant, err := w.muster.Lease(ctx)
	if err != nil {
		return false, err
	}
	if grant == nil {
		return false, nil
	}

	hbCtx, stopHB := context.WithCancel(ctx)
	doneHB := make(chan struct{})
	go func() {
		defer close(doneHB)
		w.heartbeatLoop(hbCtx, grant.Lease)
	}()

	entry := w.evaluateGrant(ctx, grant)
	if _, err := w.spool.Enqueue(entry); err != nil {
		stopHB()
		<-doneHB
		return true, err
	}

	flushErr := w.spool.Flush(ctx, w.muster, w.logf)
	stopHB()
	<-doneHB
	return true, flushErr
}

func (w *Worker) heartbeatLoop(ctx context.Context, lease string) {
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := w.muster.Heartbeat(hbCtx, lease)
			cancel()
			if err != nil && !muster.IsRetryable(err) {
				return
			}
		}
	}
}

func (w *Worker) evaluateGrant(ctx context.Context, grant *muster.Grant) spool.Entry {
	if grant.Kind != "item" {
		return spool.Entry{
			ItemID: grant.ID,
			Lease:  grant.Lease,
			Call:   spool.CallFail,
			Code:   watch.CodeParse,
		}
	}

	switch grant.State {
	case "live", "linked", "parked":
	default:
		return spool.Entry{
			ItemID: grant.ID,
			Lease:  grant.Lease,
			Call:   spool.CallFail,
			Code:   watch.CodeParse,
		}
	}

	targets, prevByID, ok := extractTargetsAndBaseline(grant)
	if !ok || len(targets) == 0 {
		return spool.Entry{
			ItemID: grant.ID,
			Lease:  grant.Lease,
			Call:   spool.CallNeedsSubtool,
		}
	}

	var (
		nextBaselines = make([]TargetState, 0, len(targets))
		toolsChecked  []string
		driftTools    []string
		signals       []string
		anyChanged    bool
		anyRepealed   bool
		superseded    bool
	)

	for _, tgt := range targets {
		toolsChecked = append(toolsChecked, tgt.tool)
		prev, hasPrev := prevByID[tgt.id]

		curState, changed, targetSignals, targetSuperseded, werr := w.checkTarget(ctx, tgt, prev, hasPrev)
		if werr != nil {
			switch werr.Code {
			case watch.CodeTimeout:
				return spool.Entry{
					ItemID: grant.ID,
					Lease:  grant.Lease,
					Call:   spool.CallFail,
					Code:   watch.CodeTimeout,
				}
			case watch.CodeNeedsSubtool:
				return spool.Entry{
					ItemID: grant.ID,
					Lease:  grant.Lease,
					Call:   spool.CallNeedsSubtool,
				}
			default:
				return spool.Entry{
					ItemID: grant.ID,
					Lease:  grant.Lease,
					Call:   spool.CallFail,
					Code:   watch.CodeParse,
				}
			}
		}

		nextBaselines = append(nextBaselines, curState)
		if changed {
			anyChanged = true
			driftTools = append(driftTools, tgt.tool)
		}
		if curState.Repealed {
			anyRepealed = true
			if targetSuperseded {
				superseded = true
			}
		}
		signals = append(signals, targetSignals...)
	}

	res := StoredResult{
		Status:       watch.StatusUnchanged,
		ToolsChecked: watch.SortedUnique(toolsChecked),
		DriftTools:   watch.SortedUnique(driftTools),
		Signals:      watch.SortedUnique(signals),
		Baselines:    nextBaselines,
	}

	targetState := grant.State
	switch grant.State {
	case "live":
		switch {
		case anyRepealed:
			targetState = "retired"
			res.Status = watch.StatusChanged
			reason := "repealed"
			note := "governing rule or clause was repealed"
			if superseded {
				reason = "superseded"
				note = "governing standard or rule was superseded"
			}
			res.Retire = &RetireBlock{Reason: reason, Note: note}
		case anyChanged:
			targetState = "building"
			res.Status = watch.StatusChanged
		default:
			targetState = "live"
		}

	case "linked":
		if anyChanged {
			targetState = "observed"
			res.Status = watch.StatusChanged
		} else {
			targetState = "linked"
		}

	case "parked":
		if anyChanged {
			targetState = "observed"
			res.Status = watch.StatusChanged
		} else {
			targetState = "parked"
		}
	}

	rawRes, err := json.Marshal(res)
	if err != nil {
		return spool.Entry{
			ItemID: grant.ID,
			Lease:  grant.Lease,
			Call:   spool.CallFail,
			Code:   watch.CodeParse,
		}
	}
	return spool.Entry{
		ItemID: grant.ID,
		Lease:  grant.Lease,
		Call:   spool.CallComplete,
		State:  targetState,
		Result: rawRes,
	}
}

func (w *Worker) checkTarget(
	ctx context.Context,
	tgt watchTarget,
	prev TargetState,
	hasPrev bool,
) (cur TargetState, changed bool, signals []string, isSuperseded bool, werr *watch.Error) {
	prevETag := ""
	if hasPrev {
		prevETag = prev.ETag
	}
	resp, ferr := w.fetcher.Get(ctx, tgt.url, prevETag)
	if ferr != nil {
		return TargetState{}, false, nil, false, ferr
	}
	defer resp.Close()

	// Conditional 304 Not Modified or matching ETag on 200 OK.
	if hasPrev && (resp.NotModified || (resp.StatusCode == http.StatusOK && watch.SameETag(prev.ETag, resp.ETag))) {
		carried := prev
		if resp.ETag != "" {
			carried.ETag = resp.ETag
		}
		return carried, false, nil, false, nil
	}

	switch tgt.tool {
	case ecfr.Name:
		if werr := requireFeedOK(resp.StatusCode); werr != nil {
			return TargetState{}, false, nil, false, werr
		}
		sum, err := ecfr.Inspect(resp.Body)
		if err != nil {
			return TargetState{}, false, nil, false, err
		}
		cur = TargetState{
			ID:       tgt.id,
			Tool:     ecfr.Name,
			ETag:     resp.ETag,
			Digest:   sum.Digest,
			Repealed: sum.Reserved,
			ECFR:     &sum,
		}
		if sum.Reserved {
			signals = append(signals, "repealed")
		}
		if hasPrev && prev.Digest != sum.Digest {
			changed = true
			signals = append(signals, "regulation_changed")
		} else if !hasPrev && sum.Reserved {
			changed = true
		}
		return cur, changed, signals, false, nil

	case federalregister.Name:
		if werr := requireFeedOK(resp.StatusCode); werr != nil {
			return TargetState{}, false, nil, false, werr
		}
		sum, err := federalregister.Inspect(resp.Body)
		if err != nil {
			return TargetState{}, false, nil, false, err
		}
		cur = TargetState{
			ID:       tgt.id,
			Tool:     federalregister.Name,
			ETag:     resp.ETag,
			Digest:   sum.Digest,
			Repealed: sum.Repealed,
			FedReg:   &sum,
		}
		if sum.Repealed {
			signals = append(signals, "repealed")
		}
		if hasPrev && prev.Digest != sum.Digest {
			changed = true
			signals = append(signals, "rulemaking_changed")
		} else if !hasPrev && sum.Repealed {
			changed = true
		}
		return cur, changed, signals, false, nil

	case nistcsrc.Name:
		if werr := requireFeedOK(resp.StatusCode); werr != nil {
			return TargetState{}, false, nil, false, werr
		}
		sum, err := nistcsrc.Inspect(resp.Body)
		if err != nil {
			return TargetState{}, false, nil, false, err
		}
		cur = TargetState{
			ID:       tgt.id,
			Tool:     nistcsrc.Name,
			ETag:     resp.ETag,
			Digest:   sum.Digest,
			Repealed: sum.Repealed,
			NIST:     &sum,
		}
		if sum.Repealed {
			signals = append(signals, "repealed")
			isSuperseded = true
		}
		if hasPrev && prev.Digest != sum.Digest {
			changed = true
			signals = append(signals, "standard_changed")
		} else if !hasPrev && sum.Repealed {
			changed = true
		}
		return cur, changed, signals, isSuperseded, nil

	case linkedtoolwatch.Name:
		sum, err := linkedtoolwatch.Inspect(linkedtoolwatch.Snapshot{
			StatusCode: resp.StatusCode,
			ETag:       resp.ETag,
			Origin:     resp.Origin,
			Body:       resp.Body,
		})
		if err != nil {
			return TargetState{}, false, nil, false, err
		}
		cur = TargetState{
			ID:         tgt.id,
			Tool:       linkedtoolwatch.Name,
			ETag:       resp.ETag,
			Digest:     sum.Digest,
			LinkedTool: &sum,
		}
		if hasPrev && prev.LinkedTool != nil {
			rep := linkedtoolwatch.DiffSummaries(*prev.LinkedTool, sum)
			if len(rep.Signals) > 0 {
				changed = true
				signals = append(signals, rep.Signals...)
			}
		} else {
			if !sum.Available {
				changed = true
				signals = append(signals, linkedtoolwatch.SignalOffline)
			}
			if sum.Paywalled {
				changed = true
				signals = append(signals, linkedtoolwatch.SignalPaywall)
			}
			if len(sum.TrackerHashes) > 0 {
				changed = true
				signals = append(signals, linkedtoolwatch.SignalTrackingScript)
			}
		}
		return cur, changed, signals, false, nil

	case livecatalog.Name:
		sum, err := livecatalog.Inspect(livecatalog.Snapshot{
			StatusCode:   resp.StatusCode,
			ETag:         resp.ETag,
			ExpectedSlug: tgt.expectedSlug,
			Body:         resp.Body,
		})
		if err != nil {
			return TargetState{}, false, nil, false, err
		}
		cur = TargetState{
			ID:          tgt.id,
			Tool:        livecatalog.Name,
			ETag:        resp.ETag,
			Digest:      sum.Digest,
			Repealed:    sum.Repealed,
			LiveCatalog: &sum,
		}
		if hasPrev && prev.LiveCatalog != nil {
			rep := livecatalog.DiffSummaries(*prev.LiveCatalog, sum)
			if rep.Status == watch.StatusChanged {
				changed = true
				for _, ch := range rep.Changes {
					signals = append(signals, ch.Signals...)
					if ch.RuleStatus == "superseded" {
						isSuperseded = true
					}
				}
			}
		} else {
			if sum.Unhealthy {
				changed = true
				signals = append(signals, livecatalog.SignalHealthFailed)
			}
			if sum.Repealed {
				changed = true
				signals = append(signals, livecatalog.SignalRepealed)
			}
			if tgt.expectedRule != "" {
				for _, e := range sum.Entries {
					if e.RuleVersion != "" && e.RuleVersion != tgt.expectedRule {
						changed = true
						signals = append(signals, livecatalog.SignalRuleVersionDrift)
					}
				}
			}
		}
		return cur, changed, signals, isSuperseded, nil

	default:
		return TargetState{}, false, nil, false, &watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "no sub-tool matches this watch target",
		}
	}
}

func requireFeedOK(code int) *watch.Error {
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &watch.Error{Code: watch.CodeTimeout, Message: "upstream regulatory feed is temporarily unavailable"}
	default:
		return &watch.Error{Code: watch.CodeParse, Message: "upstream regulatory feed returned a non-200 HTTP status"}
	}
}

// extractTargetsAndBaseline inspects grant.Results and grant.Found to collect
// deterministic watch targets and any previous Tidegauge baselines.
func extractTargetsAndBaseline(grant *muster.Grant) ([]watchTarget, map[string]TargetState, bool) {
	prevByID := map[string]TargetState{}
	if rawTG, ok := grant.Results["tidegauge"]; ok && len(rawTG) > 0 {
		var prev StoredResult
		if json.Unmarshal(rawTG, &prev) == nil {
			for _, b := range prev.Baselines {
				if b.ID != "" {
					prevByID[b.ID] = b
				}
			}
		}
	}

	var expectedSlug, expectedRule string
	type explicitTarget struct {
		Tool string `json:"tool"`
		URL  string `json:"url"`
	}
	var rawTargets []explicitTarget

	addURL := func(tool, rawURL string) {
		u := strings.TrimSpace(rawURL)
		if u == "" {
			return
		}
		rawTargets = append(rawTargets, explicitTarget{Tool: strings.TrimSpace(tool), URL: u})
	}

	// Inspect Chartroom result if present.
	if rawCR, ok := grant.Results["chartroom"]; ok && len(rawCR) > 0 {
		var cr struct {
			Slug        string `json:"slug"`
			LiveURL     string `json:"live_url"`
			URL         string `json:"url"`
			RuleVersion string `json:"rule_version"`
			Link        *struct {
				URL string `json:"url"`
			} `json:"link"`
			Provenance *struct {
				Links []json.RawMessage `json:"links"`
			} `json:"provenance"`
			Targets []explicitTarget `json:"targets"`
		}
		if json.Unmarshal(rawCR, &cr) == nil {
			if cr.Slug != "" {
				expectedSlug = cr.Slug
			}
			if cr.RuleVersion != "" {
				expectedRule = cr.RuleVersion
			}
			for _, t := range cr.Targets {
				addURL(t.Tool, t.URL)
			}
			if grant.State == "linked" && cr.Link != nil && cr.Link.URL != "" {
				addURL(linkedtoolwatch.Name, cr.Link.URL)
			}
			if grant.State == "live" {
				if cr.LiveURL != "" {
					addURL(livecatalog.Name, cr.LiveURL)
				} else if cr.URL != "" {
					addURL(livecatalog.Name, cr.URL)
				}
			}
			if cr.Provenance != nil {
				for _, lraw := range cr.Provenance.Links {
					var s string
					if json.Unmarshal(lraw, &s) == nil && s != "" {
						addURL("", s)
						continue
					}
					var obj struct {
						URL  string `json:"url"`
						Href string `json:"href"`
						Tool string `json:"tool"`
					}
					if json.Unmarshal(lraw, &obj) == nil {
						if obj.URL != "" {
							addURL(obj.Tool, obj.URL)
						} else if obj.Href != "" {
							addURL(obj.Tool, obj.Href)
						}
					}
				}
			}
		}
	}

	// Inspect Waybill, Countersign, Shakedown, Sextant, Trawl for cited sources or blockers.
	for _, workerName := range []string{"waybill", "countersign", "shakedown", "sextant", "trawl"} {
		raw, ok := grant.Results[workerName]
		if !ok || len(raw) == 0 {
			continue
		}
		var doc struct {
			Slug        string           `json:"slug"`
			RuleVersion string           `json:"rule_version"`
			LiveURL     string           `json:"live_url"`
			WatchURL    string           `json:"watch_url"`
			BlockerURL  string           `json:"blocker_url"`
			LinkedURL   string           `json:"linked_url"`
			Sources     []string         `json:"sources"`
			Targets     []explicitTarget `json:"targets"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		if expectedSlug == "" && doc.Slug != "" {
			expectedSlug = doc.Slug
		}
		if expectedRule == "" && doc.RuleVersion != "" {
			expectedRule = doc.RuleVersion
		}
		for _, t := range doc.Targets {
			addURL(t.Tool, t.URL)
		}
		for _, src := range doc.Sources {
			addURL("", src)
		}
		if doc.WatchURL != "" {
			addURL("", doc.WatchURL)
		}
		if doc.BlockerURL != "" {
			addURL("", doc.BlockerURL)
		}
		if grant.State == "live" && doc.LiveURL != "" {
			addURL(livecatalog.Name, doc.LiveURL)
		}
		if grant.State == "linked" && doc.LinkedURL != "" {
			addURL(linkedtoolwatch.Name, doc.LinkedURL)
		}
	}

	if len(rawTargets) == 0 && grant.Found != nil && strings.TrimSpace(grant.Found.Address) != "" {
		addURL("", grant.Found.Address)
	}

	if len(rawTargets) == 0 {
		return nil, prevByID, false
	}

	seen := map[string]bool{}
	var out []watchTarget
	for _, rt := range rawTargets {
		tool := rt.Tool
		if tool == "" {
			tool = matchTool(rt.URL, grant.State)
		}
		if !knownSubTool(tool) {
			return nil, prevByID, false
		}
		id := targetHash(tool, rt.URL)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, watchTarget{
			id:           id,
			tool:         tool,
			url:          rt.URL,
			expectedSlug: expectedSlug,
			expectedRule: expectedRule,
		})
	}
	return out, prevByID, true
}

func matchTool(rawURL string, itemState string) string {
	switch {
	case ecfr.MatchURL(rawURL):
		return ecfr.Name
	case federalregister.MatchURL(rawURL):
		return federalregister.Name
	case nistcsrc.MatchURL(rawURL):
		return nistcsrc.Name
	case itemState == "live" && livecatalog.MatchURL(rawURL):
		return livecatalog.Name
	case itemState == "linked" && linkedtoolwatch.MatchURL(rawURL):
		return linkedtoolwatch.Name
	default:
		return ""
	}
}

func knownSubTool(name string) bool {
	switch name {
	case ecfr.Name, federalregister.Name, nistcsrc.Name, linkedtoolwatch.Name, livecatalog.Name:
		return true
	}
	return false
}

func targetHash(tool, rawURL string) string {
	sum := sha256.Sum256([]byte(tool + "\x00" + strings.TrimSpace(rawURL)))
	return hex.EncodeToString(sum[:12])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
