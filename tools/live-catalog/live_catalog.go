// Package livecatalog is the Tidegauge sub-tool that checks ClearedStack tools
// published as "live" for health failures, rule-version drift and repealed or
// superseded governing rules.
//
// Reports carry only validated catalog slugs, lifecycle statuses, rule-version
// identifiers, canonical citations and drift signal names.
package livecatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"sort"
	"strings"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Name is this sub-tool's name in component.json and tools/registry.yaml.
const Name = "live-catalog"

// Detection methods.
const (
	MethodETag       = "etag"
	MethodHTTPStatus = "http-status"
	MethodJSONDiff   = "json-diff"
	MethodDOMDiff    = "dom-diff"
)

// Signals and change kinds.
const (
	KindAdded    = "added"
	KindRemoved  = "removed"
	KindModified = "modified"

	SignalHealthFailed     = "health_failed"
	SignalRuleVersionDrift = "rule_version_drift"
	SignalRepealed         = "repealed"
)

var maxSourceBytes = 4 << 20

// Snapshot is one saved or fetched version of a live catalog manifest or tool page.
// When StatusCode is 0, 200 OK is assumed.
type Snapshot struct {
	StatusCode   int
	ETag         string
	ExpectedSlug string
	Body         io.Reader
}

// Report is the deterministic output of one live-catalog check.
type Report struct {
	Tool       string       `json:"tool"`
	Status     string       `json:"status"`
	Method     string       `json:"method,omitempty"`
	HTTPStatus int          `json:"http_status,omitempty"`
	Changes    []Change     `json:"changes,omitempty"`
	Error      *watch.Error `json:"error,omitempty"`
}

// Change describes drift detected on one live ClearedStack catalog entry.
type Change struct {
	Slug             string   `json:"slug"`
	Kind             string   `json:"kind"`
	Healthy          bool     `json:"healthy"`
	OldRuleVersion   string   `json:"old_rule_version,omitempty"`
	NewRuleVersion   string   `json:"new_rule_version,omitempty"`
	RuleStatus       string   `json:"rule_status,omitempty"`
	Repealed         bool     `json:"repealed,omitempty"`
	Signals          []string `json:"signals,omitempty"`
	CitationsRemoved []string `json:"citations_removed,omitempty"`
	CitationsAdded   []string `json:"citations_added,omitempty"`
}

// EntrySummary is the compact, source-text-free state of a single catalog entry.
type EntrySummary struct {
	Slug        string   `json:"slug"`
	Healthy     bool     `json:"healthy"`
	Status      string   `json:"status"`
	RuleVersion string   `json:"rule_version,omitempty"`
	RuleStatus  string   `json:"rule_status,omitempty"`
	Citations   []string `json:"citations,omitempty"`
	Repealed    bool     `json:"repealed,omitempty"`
}

// Summary is a compact baseline fingerprint stored by the worker.
type Summary struct {
	Format     string         `json:"format"`
	HTTPStatus int            `json:"http_status"`
	Entries    []EntrySummary `json:"entries,omitempty"`
	Digest     string         `json:"digest"`
	Repealed   bool           `json:"repealed,omitempty"`
	Unhealthy  bool           `json:"unhealthy,omitempty"`
}

// Compare checks a new live-catalog snapshot against an earlier or expected one.
func Compare(old, new Snapshot) Report {
	oldCode := normStatus(old.StatusCode)
	newCode := normStatus(new.StatusCode)

	if oldCode == 200 && newCode == 200 && watch.SameETag(old.ETag, new.ETag) {
		return Report{
			Tool:       Name,
			Status:     watch.StatusUnchanged,
			Method:     MethodETag,
			HTTPStatus: 200,
		}
	}

	if werr := statusError(newCode); werr != nil {
		return failed(werr)
	}
	if werr := statusError(oldCode); werr != nil {
		return failed(werr)
	}

	oldSum, werr := Inspect(old)
	if werr != nil {
		return failed(werr)
	}
	// Carry forward single-entry slug expectation if HTTP 404/410 has no body.
	if new.ExpectedSlug == "" && len(oldSum.Entries) == 1 {
		new.ExpectedSlug = oldSum.Entries[0].Slug
	}
	newSum, werr := Inspect(new)
	if werr != nil {
		return failed(werr)
	}
	return DiffSummaries(oldSum, newSum)
}

// DiffSummaries compares two Summaries and produces a deterministic Report.
func DiffSummaries(oldSum, newSum Summary) Report {
	method := MethodJSONDiff
	switch {
	case newSum.HTTPStatus != 200 || oldSum.HTTPStatus != 200:
		method = MethodHTTPStatus
	case oldSum.Format != newSum.Format:
		return failed(&watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the two versions do not share the same live-catalog format",
		})
	case newSum.Format == "dom":
		method = MethodDOMDiff
	}

	oldBySlug := make(map[string]EntrySummary, len(oldSum.Entries))
	oldOrder := make(map[string]int, len(oldSum.Entries))
	for i, e := range oldSum.Entries {
		oldBySlug[e.Slug] = e
		oldOrder[e.Slug] = i
	}
	newBySlug := make(map[string]EntrySummary, len(newSum.Entries))
	seen := make(map[string]bool, len(oldSum.Entries))
	var beforeSlugs []string
	var added []EntrySummary
	for _, e := range newSum.Entries {
		newBySlug[e.Slug] = e
		prev, ok := oldBySlug[e.Slug]
		if !ok {
			added = append(added, e)
			continue
		}
		seen[e.Slug] = true
		if !sameEntry(prev, e) {
			beforeSlugs = append(beforeSlugs, e.Slug)
		}
	}
	for _, e := range oldSum.Entries {
		if !seen[e.Slug] {
			beforeSlugs = append(beforeSlugs, e.Slug)
		}
	}
	if len(beforeSlugs) == 0 && len(added) == 0 {
		return Report{
			Tool:       Name,
			Status:     watch.StatusUnchanged,
			Method:     method,
			HTTPStatus: newSum.HTTPStatus,
		}
	}
	sort.Slice(beforeSlugs, func(i, j int) bool { return oldOrder[beforeSlugs[i]] < oldOrder[beforeSlugs[j]] })

	changes := make([]Change, 0, len(beforeSlugs)+len(added))
	for _, slug := range beforeSlugs {
		prev := oldBySlug[slug]
		cur, ok := newBySlug[slug]
		if !ok {
			changes = append(changes, Change{
				Slug:             prev.Slug,
				Kind:             KindRemoved,
				Healthy:          false,
				OldRuleVersion:   prev.RuleVersion,
				RuleStatus:       prev.RuleStatus,
				Repealed:         prev.Repealed,
				Signals:          []string{SignalHealthFailed},
				CitationsRemoved: prev.Citations,
			})
			continue
		}
		var signals []string
		if !cur.Healthy {
			signals = append(signals, SignalHealthFailed)
		}
		versionCompared := newSum.HTTPStatus == 200
		var citRm, citAdd []string
		var oldRV, newRV string
		if versionCompared {
			citRm, citAdd = watch.MultisetDiff(prev.Citations, cur.Citations)
			if prev.RuleVersion != cur.RuleVersion || len(citRm) > 0 || len(citAdd) > 0 {
				signals = append(signals, SignalRuleVersionDrift)
			}
			if prev.RuleVersion != cur.RuleVersion {
				oldRV = prev.RuleVersion
				newRV = cur.RuleVersion
			}
		}
		if !prev.Repealed && cur.Repealed {
			signals = append(signals, SignalRepealed)
		}
		changes = append(changes, Change{
			Slug:             cur.Slug,
			Kind:             KindModified,
			Healthy:          cur.Healthy,
			OldRuleVersion:   oldRV,
			NewRuleVersion:   newRV,
			RuleStatus:       cur.RuleStatus,
			Repealed:         cur.Repealed,
			Signals:          signals,
			CitationsRemoved: citRm,
			CitationsAdded:   citAdd,
		})
	}
	for _, cur := range added {
		var signals []string
		if !cur.Healthy {
			signals = append(signals, SignalHealthFailed)
		}
		if cur.Repealed {
			signals = append(signals, SignalRepealed)
		}
		changes = append(changes, Change{
			Slug:           cur.Slug,
			Kind:           KindAdded,
			Healthy:        cur.Healthy,
			NewRuleVersion: cur.RuleVersion,
			RuleStatus:     cur.RuleStatus,
			Repealed:       cur.Repealed,
			Signals:        signals,
			CitationsAdded: cur.Citations,
		})
	}

	return Report{
		Tool:       Name,
		Status:     watch.StatusChanged,
		Method:     method,
		HTTPStatus: newSum.HTTPStatus,
		Changes:    changes,
	}
}

// Inspect validates a live-catalog snapshot and returns its Summary.
func Inspect(snap Snapshot) (Summary, *watch.Error) {
	code := normStatus(snap.StatusCode)
	if werr := statusError(code); werr != nil {
		return Summary{}, werr
	}
	if code == 404 || code == 410 {
		slug := strings.TrimSpace(snap.ExpectedSlug)
		if slug == "" || !validSlug(slug) {
			slug = "unknown-live-tool"
		}
		entries := []EntrySummary{{
			Slug:       slug,
			Healthy:    false,
			Status:     "unhealthy",
			RuleStatus: "active",
		}}
		return finalizeSummary("http", code, entries), nil
	}
	if code != 200 {
		return Summary{}, &watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the live tool returned an unhandled HTTP status code",
		}
	}

	data, werr := watch.ReadBounded(snap.Body, maxSourceBytes)
	if werr != nil {
		return Summary{}, werr
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Summary{}, &watch.Error{Code: watch.CodeParse, Message: "the live-catalog response is empty"}
	}
	switch trimmed[0] {
	case '{', '[':
		entries, err := parseJSON(trimmed)
		if err != nil {
			return Summary{}, err
		}
		return finalizeSummary("json", 200, entries), nil
	case '<':
		entries, err := parseDOM(trimmed)
		if err != nil {
			return Summary{}, err
		}
		return finalizeSummary("dom", 200, entries), nil
	default:
		return Summary{}, &watch.Error{Code: watch.CodeParse, Message: "the live-catalog response is neither JSON nor HTML"}
	}
}

func finalizeSummary(format string, code int, entries []EntrySummary) Summary {
	sorted := append([]EntrySummary(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	repealed := false
	unhealthy := false
	for _, e := range sorted {
		write(e.Slug)
		if e.Healthy {
			write("healthy:1")
		} else {
			write("healthy:0")
			unhealthy = true
		}
		write(e.Status)
		write(e.RuleVersion)
		write(e.RuleStatus)
		for _, c := range e.Citations {
			write("cit:" + c)
		}
		if e.Repealed {
			repealed = true
		}
	}
	return Summary{
		Format:     format,
		HTTPStatus: code,
		Entries:    entries,
		Digest:     hex.EncodeToString(h.Sum(nil)[:16]),
		Repealed:   repealed,
		Unhealthy:  unhealthy,
	}
}

func sameEntry(a, b EntrySummary) bool {
	if a.Slug != b.Slug || a.Healthy != b.Healthy || a.Status != b.Status ||
		a.RuleVersion != b.RuleVersion || a.RuleStatus != b.RuleStatus || a.Repealed != b.Repealed {
		return false
	}
	if len(a.Citations) != len(b.Citations) {
		return false
	}
	for i := range a.Citations {
		if a.Citations[i] != b.Citations[i] {
			return false
		}
	}
	return true
}

func normStatus(code int) int {
	if code == 0 {
		return 200
	}
	return code
}

func statusError(code int) *watch.Error {
	switch code {
	case 200, 404, 410:
		return nil
	case 408, 429, 502, 503, 504:
		return &watch.Error{Code: watch.CodeTimeout, Message: "the live tool timed out or is temporarily unavailable"}
	case 500:
		return &watch.Error{Code: watch.CodeParse, Message: "the live tool returned an internal server error"}
	}
	if code < 200 || code >= 500 {
		return &watch.Error{Code: watch.CodeParse, Message: "the live tool returned an invalid HTTP status"}
	}
	return &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the live tool returned an unhandled HTTP status code"}
}

func failed(err *watch.Error) Report {
	return Report{Tool: Name, Status: watch.StatusFor(err), Error: err}
}

type jsonEntry struct {
	Slug        string   `json:"slug"`
	Status      string   `json:"status"`
	Healthy     *bool    `json:"healthy"`
	RuleVersion string   `json:"rule_version"`
	RuleStatus  string   `json:"rule_status"`
	Citations   []string `json:"citations"`
}

func parseJSON(data []byte) ([]EntrySummary, *watch.Error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		var probe any
		if json.Unmarshal(data, &probe) == nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the JSON source is not a live-catalog object"}
		}
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the source is not well-formed JSON"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the source has trailing content after JSON"}
	}

	var list []jsonEntry
	switch {
	case raw["entries"] != nil:
		if err := json.Unmarshal(raw["entries"], &list); err != nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the live-catalog entries list has an unfamiliar shape"}
		}
	case raw["tools"] != nil:
		if err := json.Unmarshal(raw["tools"], &list); err != nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the live-catalog tools list has an unfamiliar shape"}
		}
	case raw["slug"] != nil:
		var single jsonEntry
		if err := json.Unmarshal(data, &single); err != nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the live-catalog entry has an unfamiliar shape"}
		}
		list = []jsonEntry{single}
	default:
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the JSON object is not a live-catalog manifest"}
	}
	if len(list) == 0 {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the live-catalog manifest contains no entries"}
	}

	seen := make(map[string]bool, len(list))
	out := make([]EntrySummary, 0, len(list))
	for _, je := range list {
		slug := strings.TrimSpace(je.Slug)
		if !validSlug(slug) {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a live-catalog entry has a missing or invalid slug"}
		}
		if seen[slug] {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "two live-catalog entries share the same slug"}
		}
		seen[slug] = true

		catStatus, ok := normalizeCatalogStatus(je.Status)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a live-catalog entry has an unknown status"}
		}
		ruleStatus, ok := normalizeRuleStatus(je.RuleStatus)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a live-catalog entry has an unknown rule_status"}
		}

		healthy := catStatus == "live"
		if je.Healthy != nil && !*je.Healthy {
			healthy = false
		}
		repealed := catStatus == "retired" || ruleStatus == "repealed" || ruleStatus == "superseded"
		var cits []string
		for _, c := range je.Citations {
			if n := watch.Normalise(c); n != "" {
				cits = append(cits, n)
			}
		}
		out = append(out, EntrySummary{
			Slug:        slug,
			Healthy:     healthy,
			Status:      catStatus,
			RuleVersion: watch.Normalise(je.RuleVersion),
			RuleStatus:  ruleStatus,
			Citations:   watch.SortedUnique(cits),
			Repealed:    repealed,
		})
	}
	return out, nil
}

func parseDOM(data []byte) ([]EntrySummary, *watch.Error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var (
		seenHTML    bool
		closedHTML  bool
		depth       int
		slug        string
		status      string
		ruleVersion string
		ruleStatus  string
		healthyAttr string
		citations   []string
	)

	attrVal := func(attrs []xml.Attr, name string) string {
		for _, a := range attrs {
			if strings.EqualFold(a.Name.Local, name) {
				return strings.TrimSpace(a.Value)
			}
		}
		return ""
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &watch.Error{Code: watch.CodeParse, Message: "the HTML source is malformed"}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			tag := strings.ToLower(t.Name.Local)
			if !seenHTML {
				if tag != "html" {
					return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the source is not an HTML live tool page"}
				}
				seenHTML = true
			}
			depth++
			if tag == "meta" {
				name := strings.ToLower(attrVal(t.Attr, "name"))
				content := attrVal(t.Attr, "content")
				switch name {
				case "cs:slug":
					slug = content
				case "cs:status":
					status = content
				case "cs:healthy":
					healthyAttr = content
				case "cs:rule-version":
					ruleVersion = content
				case "cs:rule-status":
					ruleStatus = content
				case "cs:citation":
					if n := watch.Normalise(content); n != "" {
						citations = append(citations, n)
					}
				}
			}
		case xml.EndElement:
			depth--
			if strings.EqualFold(t.Name.Local, "html") && depth == 0 {
				closedHTML = true
			}
		}
	}

	if !seenHTML || !closedHTML {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the HTML source is incomplete or truncated"}
	}
	if slug == "" || !validSlug(slug) {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the HTML page is missing a valid cs:slug meta tag"}
	}
	catStatus, ok := normalizeCatalogStatus(status)
	if !ok {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the HTML page carries an unknown cs:status value"}
	}
	rStatus, ok := normalizeRuleStatus(ruleStatus)
	if !ok {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the HTML page carries an unknown cs:rule-status value"}
	}
	healthy := catStatus == "live"
	if strings.EqualFold(healthyAttr, "false") || healthyAttr == "0" {
		healthy = false
	}
	repealed := catStatus == "retired" || rStatus == "repealed" || rStatus == "superseded"
	return []EntrySummary{{
		Slug:        slug,
		Healthy:     healthy,
		Status:      catStatus,
		RuleVersion: watch.Normalise(ruleVersion),
		RuleStatus:  rStatus,
		Citations:   watch.SortedUnique(citations),
		Repealed:    repealed,
	}}, nil
}
