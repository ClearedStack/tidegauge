package muster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

var workerNames = map[string]bool{
	"trawl":       true,
	"sextant":     true,
	"assay":       true,
	"countersign": true,
	"waybill":     true,
	"moldloft":    true,
	"shakedown":   true,
	"chartroom":   true,
	"tidegauge":   true,
	"bellbuoy":    true,
}

type routeSpec struct {
	worker string
	kind   string
	from   string
	to     string
	next   string
	lane   string
	delay  time.Duration
}

// contractRoutes mirrors CONTRACT.md "Queue steps" literally.
var contractRoutes = []routeSpec{
	{worker: "trawl", kind: "item", from: "discovered", to: "recorded", next: "sextant"},
	{worker: "trawl", kind: "item", from: "discovered", to: "rejected"},
	{worker: "sextant", kind: "item", from: "recorded", to: "observed", next: "assay"},
	{worker: "assay", kind: "item", from: "observed", to: "scored", next: "countersign"},
	{worker: "assay", kind: "item", from: "observed", to: "screened_out", next: "countersign"},
	{worker: "countersign", kind: "item", from: "scored", to: "approved", next: "waybill"},
	{worker: "countersign", kind: "item", from: "scored", to: "parked"},
	{worker: "countersign", kind: "item", from: "scored", to: "linked", next: "chartroom"},
	{worker: "countersign", kind: "item", from: "scored", to: "merged", next: "chartroom"},
	{worker: "countersign", kind: "item", from: "scored", to: "rejected"},
	{worker: "countersign", kind: "item", from: "screened_out", to: "observed", next: "assay"},
	{worker: "waybill", kind: "item", from: "approved", to: "approved", next: "moldloft"},
	{worker: "moldloft", kind: "item", from: "approved", to: "building", next: "shakedown"},
	{worker: "shakedown", kind: "item", from: "building", to: "building", next: "chartroom"},
	{worker: "shakedown", kind: "item", from: "building", to: "parked"},
	{worker: "chartroom", kind: "item", from: "building", to: "live", next: "tidegauge", lane: "backfill", delay: 168 * time.Hour},
	{worker: "chartroom", kind: "item", from: "linked", to: "linked"},
	{worker: "chartroom", kind: "item", from: "merged", to: "merged"},
	{worker: "chartroom", kind: "item", from: "retired", to: "retired"},
	{worker: "tidegauge", kind: "item", from: "live", to: "live", next: "tidegauge", lane: "backfill", delay: 168 * time.Hour},
	{worker: "tidegauge", kind: "item", from: "live", to: "retired", next: "chartroom", lane: "urgent"},
	{worker: "tidegauge", kind: "item", from: "live", to: "building", next: "waybill", lane: "urgent"},
	{worker: "waybill", kind: "item", from: "building", to: "building", next: "moldloft"},
	{worker: "moldloft", kind: "item", from: "building", to: "building", next: "shakedown"},
}

// specExtendedRoutes adds the linked/parked revisit routes described in
// Tidegauge's SPEC.md for opt-in worker integration tests while keeping
// strict CONTRACT.md v1 behavior as the default.
var specExtendedRoutes = []routeSpec{
	{worker: "tidegauge", kind: "item", from: "linked", to: "linked", next: "tidegauge", lane: "backfill", delay: 168 * time.Hour},
	{worker: "tidegauge", kind: "item", from: "linked", to: "observed", next: "assay", lane: "normal"},
	{worker: "tidegauge", kind: "item", from: "parked", to: "parked", next: "tidegauge", lane: "backfill", delay: 168 * time.Hour},
	{worker: "tidegauge", kind: "item", from: "parked", to: "observed", next: "assay", lane: "normal"},
}

var pausableStatesStrict = map[string]bool{
	"recorded": true,
	"observed": true,
	"approved": true,
}

var pausableStatesExtended = map[string]bool{
	"recorded": true,
	"observed": true,
	"approved": true,
	"live":     true,
	"linked":   true,
	"parked":   true,
}

// SeedItem seeds an item into the in-memory Standin server.
type SeedItem struct {
	ID      string                     `json:"id"`
	State   string                     `json:"state"`
	Lane    string                     `json:"lane"`
	Next    string                     `json:"next,omitempty"`
	Found   *FoundView                 `json:"found,omitempty"`
	Results map[string]json.RawMessage `json:"results,omitempty"`
}

type storedItem struct {
	ID         string
	State      string
	PausedFrom string
	PausedBy   string
	Lane       string
	Found      *FoundView
	Results    map[string]json.RawMessage
}

type queueStep struct {
	Key       string
	Subject   string
	Kind      string
	Worker    string
	Lane      string
	Seq       int
	Attempt   int
	Lease     string
	Expires   time.Time
	NotBefore time.Time
	Held      bool
	Incident  *IncidentView
}

type finishedCall struct {
	Lease  string
	Worker string
	Call   string
	Arg    string
	Ack    Ack
}

// Standin is a self-contained in-memory Muster contract v1 server used for
// contract example replay and standalone worker testing.
type Standin struct {
	mu                          sync.Mutex
	tokens                      map[string]string
	clock                       func() time.Time
	now                         time.Time
	leaseFor                    time.Duration
	allowExtendedTidegaugeMoves bool
	seq                         int
	items                       map[string]*storedItem
	steps                       map[string]*queueStep
	finished                    map[string]finishedCall
}

// NewStandin creates a Standin server starting at 2026-01-05T09:00:00Z with
// 60-second leases and tokens for every standard caller ("tok-<name>").
func NewStandin(seed []SeedItem) *Standin {
	start := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	tokens := map[string]string{
		"tok-conn":       "conn",
		"tok-shipkeeper": "shipkeeper",
	}
	for w := range workerNames {
		tokens["tok-"+w] = w
	}
	s := &Standin{
		tokens:   tokens,
		now:      start,
		leaseFor: 60 * time.Second,
		items:    map[string]*storedItem{},
		steps:    map[string]*queueStep{},
		finished: map[string]finishedCall{},
	}
	s.clock = func() time.Time { return s.now }
	for _, it := range seed {
		s.AddItem(it)
	}
	return s
}

// TokenFor returns the bearer token registered for caller.
func (s *Standin) TokenFor(caller string) string {
	return "tok-" + caller
}

// Advance moves Standin's internal clock forward by d.
func (s *Standin) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

// SetAllowExtendedTidegaugeMoves toggles opt-in support for SPEC.md's
// linked/parked revisit routes and pausing live/linked/parked items.
// When false (the default), Standin enforces CONTRACT.md v1 strictly.
func (s *Standin) SetAllowExtendedTidegaugeMoves(allow bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowExtendedTidegaugeMoves = allow
}

// AddItem inserts an item and, when Next or a default worker for State exists,
// queues its step.
func (s *Standin) AddItem(seed SeedItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lane := seed.Lane
	if lane == "" {
		lane = "normal"
	}
	res := map[string]json.RawMessage{}
	for k, v := range seed.Results {
		res[k] = append(json.RawMessage(nil), v...)
	}
	s.items[seed.ID] = &storedItem{
		ID:      seed.ID,
		State:   seed.State,
		Lane:    lane,
		Found:   seed.Found,
		Results: res,
	}
	next := seed.Next
	if next == "" && seed.State == "live" {
		next = "tidegauge"
	}
	if next != "" {
		s.seq++
		s.steps[seed.ID] = &queueStep{
			Key:     seed.ID,
			Subject: seed.ID,
			Kind:    "item",
			Worker:  next,
			Lane:    lane,
			Seq:     s.seq,
		}
	}
}

// InspectItem returns an item's current state and stored results copy.
func (s *Standin) InspectItem(id string) (state string, results map[string]json.RawMessage, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, exists := s.items[id]
	if !exists {
		return "", nil, false
	}
	cp := make(map[string]json.RawMessage, len(it.Results))
	for k, v := range it.Results {
		cp[k] = append(json.RawMessage(nil), v...)
	}
	return it.State, cp, true
}

func (s *Standin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			writeRefusal(w, http.StatusMethodNotAllowed, watch.CodeParse, "wrong_method", "this call does not accept that method")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	knownMethods := map[string]string{
		PathLease:        http.MethodPost,
		PathHeartbeat:    http.MethodPost,
		PathComplete:     http.MethodPost,
		PathFail:         http.MethodPost,
		PathNeedsSubtool: http.MethodPost,
		"/v1/items":      http.MethodPost,
		"/v1/release":    http.MethodPost,
		"/v1/health":     http.MethodPost,
		"/v1/edits":      http.MethodPost,
		"/v1/feed":       http.MethodGet,
	}
	wantMethod, pathExists := knownMethods[r.URL.Path]
	if !pathExists {
		writeRefusal(w, http.StatusNotFound, watch.CodeParse, "no_such_call", "no such call")
		return
	}
	if r.Method != wantMethod {
		writeRefusal(w, http.StatusMethodNotAllowed, watch.CodeParse, "wrong_method", "this call does not accept that method")
		return
	}

	caller, ok := s.authenticate(r.Header.Get("Authorization"))
	if !ok {
		writeRefusal(w, http.StatusUnauthorized, watch.CodeParse, "no_credential", "no valid credential was presented")
		return
	}
	if !authorized(caller, r.URL.Path) {
		writeRefusal(w, http.StatusForbidden, watch.CodeParse, "not_your_call", "this caller may not make this call")
		return
	}

	if r.Method == http.MethodPost {
		ct := r.Header.Get("Content-Type")
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			writeRefusal(w, http.StatusUnsupportedMediaType, watch.CodeParse, "wrong_type", "the content type must be application/json")
			return
		}
	}

	limit := int64(MaxBodyBytes)
	if r.URL.Path == PathComplete && caller == "waybill" {
		limit = 256 << 10
	} else if r.URL.Path == "/v1/edits" {
		limit = 4096
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "not_json", "the request is not one json value")
		return
	}
	if int64(len(raw)) > limit {
		writeRefusal(w, http.StatusRequestEntityTooLarge, watch.CodeParse, "too_large", "the request is too large")
		return
	}

	switch r.URL.Path {
	case PathLease:
		s.handleLease(w, caller, raw)
	case PathHeartbeat:
		s.handleHeartbeat(w, caller, raw)
	case PathComplete:
		s.handleComplete(w, caller, raw)
	case PathFail:
		s.handleFail(w, caller, raw)
	case PathNeedsSubtool:
		s.handleNeedsSubtool(w, caller, raw)
	default:
		writeRefusal(w, http.StatusForbidden, watch.CodeParse, "not_your_call", "this caller may not make this call")
	}
}

func (s *Standin) authenticate(header string) (string, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	s.mu.Lock()
	defer s.mu.Unlock()
	caller, ok := s.tokens[tok]
	return caller, ok
}

func authorized(caller, path string) bool {
	switch path {
	case PathLease, PathHeartbeat, PathComplete, PathFail, PathNeedsSubtool:
		return workerNames[caller]
	case "/v1/items":
		return caller == "trawl" || caller == "conn"
	case "/v1/release":
		return caller == "countersign"
	case "/v1/health":
		return caller == "shipkeeper"
	case "/v1/edits", "/v1/feed":
		return caller == "conn"
	default:
		return false
	}
}

func checkJSONEnvelope(w http.ResponseWriter, raw []byte, dst any) (contractVal string, ok bool) {
	var probe any
	dec0 := json.NewDecoder(bytes.NewReader(raw))
	if err := dec0.Decode(&probe); err != nil {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "not_json", "the request is not one json value")
		return "", false
	}
	if _, err := dec0.Token(); err != io.EOF {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "not_json", "the request is not one json value")
		return "", false
	}
	obj, isObj := probe.(map[string]any)
	if !isObj {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "not_json", "the request is not one json value")
		return "", false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "unknown_field", "the request has a field this call does not define")
			return "", false
		}
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "a field does not have the form this call requires")
		return "", false
	}

	cRaw, hasContract := obj["contract"]
	if !hasContract {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return "", false
	}
	cStr, isStr := cRaw.(string)
	if !isStr {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "a field does not have the form this call requires")
		return "", false
	}
	if cStr == "" {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return "", false
	}
	if cStr != ContractVersion {
		writeRefusal(w, http.StatusUnprocessableEntity, watch.CodeNeedsSubtool, "unknown_contract", "this contract version is not known")
		return "", false
	}
	return cStr, true
}

func (s *Standin) handleLease(w http.ResponseWriter, worker string, raw []byte) {
	var req leaseReq
	if _, ok := checkJSONEnvelope(w, raw, &req); !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()

	step := s.nextAvailableStepLocked(worker, now)
	if step == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if step.Kind == "item" {
		it, exists := s.items[step.Subject]
		if !exists || !s.workerMayHoldLocked(worker, it.State) {
			step.Held = true
			step.Lease = ""
			step.Expires = time.Time{}
			if exists {
				s.openIncidentLocked(it.ID, worker, watch.CodeParse, it.State)
			}
			writeRefusal(w, http.StatusInternalServerError, watch.CodeParse, "bad_record", "a stored record was not consistent and has been set aside")
			return
		}
		step.Attempt++
		step.Lease = fmt.Sprintf("%s.%d.%d", step.Subject, step.Seq, step.Attempt)
		step.Expires = now.Add(s.leaseFor)
		var resCopy map[string]json.RawMessage
		if len(it.Results) > 0 {
			resCopy = make(map[string]json.RawMessage, len(it.Results))
			for k, v := range it.Results {
				resCopy[k] = append(json.RawMessage(nil), v...)
			}
		}
		writeJSON(w, http.StatusOK, Grant{
			Lease:   step.Lease,
			Kind:    "item",
			ID:      it.ID,
			State:   it.State,
			Lane:    step.Lane,
			Attempt: step.Attempt,
			Expires: step.Expires.UTC().Format(time.RFC3339),
			Found:   it.Found,
			Results: resCopy,
		})
		return
	}

	// Incident step for Bellbuoy.
	step.Attempt++
	step.Lease = fmt.Sprintf("%s.%d.%d", step.Key, step.Seq, step.Attempt)
	step.Expires = now.Add(s.leaseFor)
	writeJSON(w, http.StatusOK, Grant{
		Lease:    step.Lease,
		Kind:     "incident",
		ID:       step.Subject,
		State:    step.Incident.State,
		Lane:     step.Lane,
		Attempt:  step.Attempt,
		Expires:  step.Expires.UTC().Format(time.RFC3339),
		Incident: step.Incident,
	})
}

func (s *Standin) handleHeartbeat(w http.ResponseWriter, worker string, raw []byte) {
	var req heartbeatReq
	if _, ok := checkJSONEnvelope(w, raw, &req); !ok {
		return
	}
	if strings.TrimSpace(req.Lease) == "" {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	step := s.heldStepLocked(worker, req.Lease, now)
	if step == nil {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "lease_not_held", "the lease is not held")
		return
	}
	step.Expires = now.Add(s.leaseFor)
	writeJSON(w, http.StatusOK, Beat{
		Lease:   step.Lease,
		Expires: step.Expires.UTC().Format(time.RFC3339),
	})
}

func (s *Standin) handleComplete(w http.ResponseWriter, worker string, raw []byte) {
	var req completeReq
	if _, ok := checkJSONEnvelope(w, raw, &req); !ok {
		return
	}
	if strings.TrimSpace(req.Lease) == "" || strings.TrimSpace(req.State) == "" {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return
	}
	if len(req.Result) > 0 {
		trimmed := bytes.TrimSpace(req.Result)
		if string(trimmed) == "null" {
			req.Result = nil
		} else if trimmed[0] != '{' {
			writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "a field does not have the form this call requires")
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()

	if fin, ok := s.finished[req.Lease]; ok && fin.Worker == worker && fin.Call == "complete" && fin.Arg == req.State {
		writeJSON(w, http.StatusOK, fin.Ack)
		return
	}
	step := s.heldStepLocked(worker, req.Lease, now)
	if step == nil {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "lease_not_held", "the lease is not held")
		return
	}
	if step.Kind == "incident" {
		if req.State != step.Incident.State {
			writeRefusal(w, http.StatusConflict, watch.CodeParse, "move_not_allowed", "that move is not allowed")
			return
		}
		if len(req.Result) > 0 {
			writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "a field does not have the form this call requires")
			return
		}
		ack := Ack{ID: step.Subject, State: req.State}
		s.finished[req.Lease] = finishedCall{Lease: req.Lease, Worker: worker, Call: "complete", Arg: req.State, Ack: ack}
		delete(s.steps, step.Key)
		writeJSON(w, http.StatusOK, ack)
		return
	}

	it := s.items[step.Subject]
	route, ok := s.findRouteLocked(worker, "item", it.State, req.State)
	if !ok {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "move_not_allowed", "that move is not allowed")
		return
	}
	lane := step.Lane
	if route.lane != "" {
		lane = route.lane
	}
	it.State = req.State
	it.Lane = lane
	if len(req.Result) > 0 {
		if it.Results == nil {
			it.Results = map[string]json.RawMessage{}
		}
		it.Results[worker] = append(json.RawMessage(nil), req.Result...)
	}
	ack := Ack{ID: it.ID, State: req.State, Next: route.next}
	s.finished[req.Lease] = finishedCall{Lease: req.Lease, Worker: worker, Call: "complete", Arg: req.State, Ack: ack}
	delete(s.steps, step.Key)
	if route.next != "" {
		s.seq++
		var nb time.Time
		if route.delay > 0 {
			nb = now.Add(route.delay)
		}
		s.steps[it.ID] = &queueStep{
			Key:       it.ID,
			Subject:   it.ID,
			Kind:      "item",
			Worker:    route.next,
			Lane:      lane,
			Seq:       s.seq,
			NotBefore: nb,
		}
	}
	writeJSON(w, http.StatusOK, ack)
}

func (s *Standin) handleFail(w http.ResponseWriter, worker string, raw []byte) {
	var req failReq
	if _, ok := checkJSONEnvelope(w, raw, &req); !ok {
		return
	}
	if strings.TrimSpace(req.Lease) == "" || strings.TrimSpace(req.Code) == "" {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return
	}
	if req.Code == watch.CodeNeedsSubtool {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "report a missing rule with the needs_subtool call")
		return
	}
	if req.Code != watch.CodeTimeout && req.Code != watch.CodeParse {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "bad_field", "a field does not have the form this call requires")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()

	if fin, ok := s.finished[req.Lease]; ok && fin.Worker == worker && fin.Call == "fail" && fin.Arg == req.Code {
		writeJSON(w, http.StatusOK, fin.Ack)
		return
	}
	step := s.heldStepLocked(worker, req.Lease, now)
	if step == nil {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "lease_not_held", "the lease is not held")
		return
	}
	state := step.IncidentValueState(s.items)
	var ack Ack
	if req.Code == watch.CodeTimeout {
		step.Lease = ""
		step.Expires = time.Time{}
		ack = Ack{ID: step.Subject, State: state, Next: worker}
	} else {
		step.Lease = ""
		step.Expires = time.Time{}
		step.Held = true
		ack = Ack{ID: step.Subject, State: state, Held: true}
		if step.Kind == "item" {
			s.openIncidentLocked(step.Subject, worker, watch.CodeParse, state)
		}
	}
	s.finished[req.Lease] = finishedCall{Lease: req.Lease, Worker: worker, Call: "fail", Arg: req.Code, Ack: ack}
	writeJSON(w, http.StatusOK, ack)
}

func (s *Standin) handleNeedsSubtool(w http.ResponseWriter, worker string, raw []byte) {
	var req needsSubtoolReq
	if _, ok := checkJSONEnvelope(w, raw, &req); !ok {
		return
	}
	if strings.TrimSpace(req.Lease) == "" {
		writeRefusal(w, http.StatusBadRequest, watch.CodeParse, "missing_field", "a required field is missing")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()

	if fin, ok := s.finished[req.Lease]; ok && fin.Worker == worker && fin.Call == "needs_subtool" {
		writeJSON(w, http.StatusOK, fin.Ack)
		return
	}
	step := s.heldStepLocked(worker, req.Lease, now)
	if step == nil {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "lease_not_held", "the lease is not held")
		return
	}
	if step.Kind != "item" {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "move_not_allowed", "that move is not allowed")
		return
	}
	it := s.items[step.Subject]
	allowed := pausableStatesStrict[it.State]
	if s.allowExtendedTidegaugeMoves {
		allowed = pausableStatesExtended[it.State]
	}
	if !allowed {
		writeRefusal(w, http.StatusConflict, watch.CodeParse, "move_not_allowed", "that move is not allowed")
		return
	}
	prevState := it.State
	it.PausedFrom = prevState
	it.PausedBy = worker
	it.State = "needs_subtool"
	delete(s.steps, step.Key)
	s.openIncidentLocked(it.ID, worker, watch.CodeNeedsSubtool, prevState)
	ack := Ack{ID: it.ID, State: "needs_subtool"}
	s.finished[req.Lease] = finishedCall{Lease: req.Lease, Worker: worker, Call: "needs_subtool", Ack: ack}
	writeJSON(w, http.StatusOK, ack)
}

func (q *queueStep) IncidentValueState(items map[string]*storedItem) string {
	if q.Kind == "incident" && q.Incident != nil {
		return q.Incident.State
	}
	if it, ok := items[q.Subject]; ok {
		return it.State
	}
	return ""
}

func (s *Standin) openIncidentLocked(itemID, worker, code, state string) {
	s.seq++
	key := "incident:" + itemID
	s.steps[key] = &queueStep{
		Key:     key,
		Subject: itemID,
		Kind:    "incident",
		Worker:  "bellbuoy",
		Lane:    "normal",
		Seq:     s.seq,
		Incident: &IncidentView{
			ID:     itemID,
			Worker: worker,
			Code:   code,
			State:  state,
		},
	}
}

func laneRank(lane string) int {
	switch lane {
	case "urgent":
		return 0
	case "normal":
		return 1
	case "backfill":
		return 2
	default:
		return 3
	}
}

func (s *Standin) nextAvailableStepLocked(worker string, now time.Time) *queueStep {
	var best *queueStep
	for _, st := range s.steps {
		if st.Worker != worker || st.Held {
			continue
		}
		if !st.NotBefore.IsZero() && now.Before(st.NotBefore) {
			continue
		}
		if st.Lease != "" && now.Before(st.Expires) {
			continue
		}
		if best == nil || laneRank(st.Lane) < laneRank(best.Lane) ||
			(laneRank(st.Lane) == laneRank(best.Lane) && st.Seq < best.Seq) {
			best = st
		}
	}
	return best
}

// Note: in CONTRACT.md ("Leases"), until someone else takes an expired step, a
// late lease still counts so a finished result that arrives late is kept.
func (s *Standin) heldStepLocked(worker, lease string, _ time.Time) *queueStep {
	for _, st := range s.steps {
		if st.Worker == worker && st.Lease == lease && !st.Held {
			return st
		}
	}
	return nil
}

func (s *Standin) workerMayHoldLocked(worker, state string) bool {
	for _, r := range contractRoutes {
		if r.worker == worker && r.from == state {
			return true
		}
	}
	if s.allowExtendedTidegaugeMoves {
		for _, r := range specExtendedRoutes {
			if r.worker == worker && r.from == state {
				return true
			}
		}
	}
	return false
}

func (s *Standin) findRouteLocked(worker, kind, from, to string) (routeSpec, bool) {
	for _, r := range contractRoutes {
		if r.worker == worker && r.kind == kind && r.from == from && r.to == to {
			return r, true
		}
	}
	if s.allowExtendedTidegaugeMoves {
		for _, r := range specExtendedRoutes {
			if r.worker == worker && r.kind == kind && r.from == from && r.to == to {
				return r, true
			}
		}
	}
	return routeSpec{}, false
}

func writeRefusal(w http.ResponseWriter, status int, code, cause, reason string) {
	writeJSON(w, status, Refusal{
		Error:  code,
		Cause:  cause,
		Reason: reason,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
