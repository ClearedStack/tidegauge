// Package linkedtoolwatch is the Tidegauge sub-tool that watches external
// free tools marked "linked" in the ClearedStack catalog for dead links, new
// paywalls and added tracking scripts.
//
// Clean-room rule: reports and summaries carry only HTTP status codes, signal
// identifiers, counts and short irreversible hashes. Raw external HTML, prose,
// script URLs and hostnames are never retained or emitted.
package linkedtoolwatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Name is this sub-tool's name in component.json and tools/registry.yaml.
const Name = "linked-tool-watch"

// Detection methods.
const (
	MethodETag       = "etag"
	MethodHTTPStatus = "http-status"
	MethodDOMDiff    = "dom-diff"
)

// Drift signals that reopen a linked item to "observed".
const (
	SignalOffline        = "offline"
	SignalPaywall        = "paywall"
	SignalTrackingScript = "tracking_script"
)

var maxSourceBytes = 4 << 20

// Snapshot is one observed HTTP response from an external linked tool.
// When StatusCode is 0, 200 OK is assumed.
type Snapshot struct {
	StatusCode int
	ETag       string
	Origin     string
	Body       io.Reader
}

// Report is the deterministic, sanitized result of one linked-tool check.
type Report struct {
	Tool                   string       `json:"tool"`
	Status                 string       `json:"status"`
	Method                 string       `json:"method,omitempty"`
	HTTPStatus             int          `json:"http_status,omitempty"`
	Signals                []string     `json:"signals,omitempty"`
	Paywalled              bool         `json:"paywalled,omitempty"`
	TrackersAdded          int          `json:"trackers_added,omitempty"`
	TrackersRemoved        int          `json:"trackers_removed,omitempty"`
	ThirdPartyScriptsAdded int          `json:"third_party_scripts_added,omitempty"`
	Error                  *watch.Error `json:"error,omitempty"`
}

// Summary is a compact, hostname-free baseline fingerprint stored by the
// worker inside Muster's result limit.
type Summary struct {
	HTTPStatus        int      `json:"http_status"`
	Available         bool     `json:"available"`
	Paywalled         bool     `json:"paywalled,omitempty"`
	TrackerHashes     []string `json:"tracker_hashes,omitempty"`
	ThirdPartyScripts []string `json:"third_party_scripts,omitempty"`
	Digest            string   `json:"digest"`
}

// Compare checks a new linked-tool snapshot against an earlier baseline.
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

	// Check new snapshot first when its HTTP status alone is an error, so
	// transient upstream timeouts or HTTP 500s surface cleanly.
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
	newSum, werr := Inspect(new)
	if werr != nil {
		return failed(werr)
	}
	return DiffSummaries(oldSum, newSum)
}

// DiffSummaries compares two sanitized Summaries and produces a deterministic Report.
func DiffSummaries(oldSum, newSum Summary) Report {
	method := MethodDOMDiff
	if newSum.HTTPStatus != 200 || oldSum.HTTPStatus != 200 {
		method = MethodHTTPStatus
	}

	var signals []string
	if oldSum.Available && !newSum.Available {
		signals = append(signals, SignalOffline)
	}
	if !oldSum.Paywalled && newSum.Paywalled {
		signals = append(signals, SignalPaywall)
	}

	trRemoved, trAdded := watch.MultisetDiff(oldSum.TrackerHashes, newSum.TrackerHashes)
	_, tpAdded := watch.MultisetDiff(oldSum.ThirdPartyScripts, newSum.ThirdPartyScripts)
	if len(trAdded) > 0 {
		signals = append(signals, SignalTrackingScript)
	}

	if len(signals) == 0 && len(trRemoved) == 0 && oldSum.Available == newSum.Available && oldSum.Paywalled == newSum.Paywalled {
		return Report{
			Tool:       Name,
			Status:     watch.StatusUnchanged,
			Method:     method,
			HTTPStatus: newSum.HTTPStatus,
			Paywalled:  newSum.Paywalled,
		}
	}

	// If a tracker was removed or availability recovered without new negative
	// drift signals, still report the state change deterministically.
	return Report{
		Tool:                   Name,
		Status:                 watch.StatusChanged,
		Method:                 method,
		HTTPStatus:             newSum.HTTPStatus,
		Signals:                signals,
		Paywalled:              newSum.Paywalled,
		TrackersAdded:          len(trAdded),
		TrackersRemoved:        len(trRemoved),
		ThirdPartyScriptsAdded: len(tpAdded),
	}
}

// Inspect validates one Snapshot and returns its sanitized Summary.
func Inspect(snap Snapshot) (Summary, *watch.Error) {
	code := normStatus(snap.StatusCode)
	if werr := statusError(code); werr != nil {
		return Summary{}, werr
	}
	switch code {
	case 404, 410:
		return buildSummary(code, false, false, nil, nil), nil
	case 402:
		return buildSummary(code, true, true, nil, nil), nil
	case 200:
		// Parse HTML body below.
	default:
		return Summary{}, &watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the linked tool returned an unhandled HTTP status code",
		}
	}

	data, werr := watch.ReadBounded(snap.Body, maxSourceBytes)
	if werr != nil {
		return Summary{}, werr
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Summary{}, &watch.Error{Code: watch.CodeParse, Message: "the linked tool response body is empty"}
	}
	if !startsLikeHTML(trimmed) {
		if trimmed[0] == '{' || trimmed[0] == '[' || strings.HasPrefix(string(trimmed), "<?xml") {
			return Summary{}, &watch.Error{
				Code:    watch.CodeNeedsSubtool,
				Message: "the linked tool did not return an HTML document",
			}
		}
		return Summary{}, &watch.Error{Code: watch.CodeParse, Message: "the linked tool response is not valid HTML"}
	}

	paywalled, trackers, thirdParty, toolContent, werr := scanHTML(trimmed, snap.Origin)
	if werr != nil {
		return Summary{}, werr
	}
	if !toolContent && !paywalled {
		return Summary{}, &watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the HTML page has no recognizable tool structure",
		}
	}
	return buildSummary(200, true, paywalled, trackers, thirdParty), nil
}

func buildSummary(code int, available, paywalled bool, trackers, thirdParty []string) Summary {
	tr := watch.SortedUnique(trackers)
	tp := watch.SortedUnique(thirdParty)
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	if available {
		write("avail:1")
	} else {
		write("avail:0")
	}
	if paywalled {
		write("pay:1")
	} else {
		write("pay:0")
	}
	for _, x := range tr {
		write("tr:" + x)
	}
	for _, x := range tp {
		write("tp:" + x)
	}
	return Summary{
		HTTPStatus:        code,
		Available:         available,
		Paywalled:         paywalled,
		TrackerHashes:     tr,
		ThirdPartyScripts: tp,
		Digest:            hex.EncodeToString(h.Sum(nil)[:16]),
	}
}

func normStatus(code int) int {
	if code == 0 {
		return 200
	}
	return code
}

func statusError(code int) *watch.Error {
	switch code {
	case 200, 402, 404, 410:
		return nil
	case 408, 429, 502, 503, 504:
		return &watch.Error{Code: watch.CodeTimeout, Message: "the linked tool timed out or is temporarily unavailable"}
	case 500:
		return &watch.Error{Code: watch.CodeParse, Message: "the linked tool returned an internal server error"}
	}
	if code < 200 || code >= 500 {
		return &watch.Error{Code: watch.CodeParse, Message: "the linked tool returned an invalid HTTP status"}
	}
	return &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the linked tool returned an unhandled HTTP status code"}
}

func failed(err *watch.Error) Report {
	return Report{Tool: Name, Status: watch.StatusFor(err), Error: err}
}

func startsLikeHTML(b []byte) bool {
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(string(head))
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

// scanHTML deterministically scans an HTML5 document without retaining prose,
// tolerating inline JS/CSS bodies while rejecting truncated markup.
func scanHTML(data []byte, origin string) (paywalled bool, trackers []string, thirdParty []string, hasToolContent bool, werr *watch.Error) {
	var (
		seenHTML   bool
		closedHTML bool
		seenBody   bool
		closedBody bool
	)

	n := len(data)
	i := 0
	for i < n {
		lt := bytes.IndexByte(data[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if i+1 >= n {
			return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML source ends mid-tag"}
		}

		// HTML comment <!-- ... -->
		if bytes.HasPrefix(data[i:], []byte("<!--")) {
			end := bytes.Index(data[i+4:], []byte("-->"))
			if end < 0 {
				return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML source has an unclosed comment"}
			}
			i += 4 + end + 3
			continue
		}

		// Declarations like <!DOCTYPE html>
		if data[i+1] == '!' || data[i+1] == '?' {
			gt := bytes.IndexByte(data[i+2:], '>')
			if gt < 0 {
				return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML declaration is unclosed"}
			}
			i += 2 + gt + 1
			continue
		}

		// Closing tag </tag>
		if data[i+1] == '/' {
			gt := bytes.IndexByte(data[i+2:], '>')
			if gt < 0 {
				return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML closing tag is unclosed"}
			}
			tag := strings.ToLower(strings.TrimSpace(string(data[i+2 : i+2+gt])))
			if tag == "html" && seenHTML {
				closedHTML = true
			} else if tag == "body" && seenBody {
				closedBody = true
			}
			i += 2 + gt + 1
			continue
		}

		// Opening tag <tag attr="val" ...>
		tagEnd, ok := findTagEnd(data, i+1)
		if !ok {
			return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML opening tag is unclosed"}
		}
		rawTag := data[i+1 : tagEnd]
		selfClosing := len(rawTag) > 0 && rawTag[len(rawTag)-1] == '/'
		if selfClosing {
			rawTag = rawTag[:len(rawTag)-1]
		}
		tag, attrs := parseTagAttrs(rawTag)
		i = tagEnd + 1

		switch tag {
		case "html":
			seenHTML = true
		case "body":
			seenBody = true
		case "main", "form", "input", "select", "textarea", "button", "table", "article", "calculator-app":
			hasToolContent = true
		case "meta":
			if strings.EqualFold(attrs["name"], "tool-kind") || strings.EqualFold(attrs["name"], "application-name") {
				hasToolContent = true
			}
		}

		if attrs["data-tool-ready"] == "true" || attrs["role"] == "application" || attrs["role"] == "main" {
			hasToolContent = true
		}

		if hasPaywallAttr(tag, attrs) {
			paywalled = true
		}

		if tag == "script" {
			src := attrs["src"]
			if src != "" {
				ext, h := classifyScriptSrc(src, origin)
				if ext && h != "" {
					thirdParty = append(thirdParty, h)
				}
				if isTrackerScript(src, attrs) {
					if h == "" {
						h = shortHash(src)
					}
					trackers = append(trackers, h)
				}
			} else if isTrackerScript("", attrs) {
				trackers = append(trackers, shortHash("inline:"+attrs["data-tracker"]+":"+attrs["data-analytics"]))
			}

			if !selfClosing {
				closeIdx := findRawTextClose(data[i:], "script")
				if closeIdx < 0 {
					return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML script element is unclosed"}
				}
				bodyBytes := data[i : i+closeIdx]
				if strings.EqualFold(strings.TrimSpace(attrs["type"]), "application/ld+json") {
					pw, jerr := inspectJSONLD(bodyBytes)
					if jerr != nil {
						return false, nil, nil, false, jerr
					}
					if pw {
						paywalled = true
					}
				}
				// Advance past </script...>
				afterClose := i + closeIdx + len("</script")
				gt := bytes.IndexByte(data[afterClose:], '>')
				if gt < 0 {
					return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML script closing tag is unclosed"}
				}
				i = afterClose + gt + 1
			}
			continue
		}

		if tag == "style" && !selfClosing {
			closeIdx := findRawTextClose(data[i:], "style")
			if closeIdx < 0 {
				return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML style element is unclosed"}
			}
			afterClose := i + closeIdx + len("</style")
			gt := bytes.IndexByte(data[afterClose:], '>')
			if gt < 0 {
				return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML style closing tag is unclosed"}
			}
			i = afterClose + gt + 1
		}
	}

	if !seenHTML || !closedHTML || (seenBody && !closedBody) {
		return false, nil, nil, false, &watch.Error{Code: watch.CodeParse, Message: "the HTML document is incomplete or truncated"}
	}
	return paywalled, trackers, thirdParty, hasToolContent, nil
}

// findTagEnd finds the closing '>' of an HTML start tag, respecting quoted attributes.
func findTagEnd(data []byte, start int) (int, bool) {
	var quote byte
	for i := start; i < len(data); i++ {
		ch := data[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '>' {
			return i, true
		}
	}
	return 0, false
}

func findRawTextClose(b []byte, tag string) int {
	needle := "</" + tag
	lower := bytes.ToLower(b)
	pos := 0
	for {
		idx := bytes.Index(lower[pos:], []byte(needle))
		if idx < 0 {
			return -1
		}
		cand := pos + idx + len(needle)
		if cand < len(lower) {
			next := lower[cand]
			if next == '>' || next == ' ' || next == '\t' || next == '\r' || next == '\n' {
				return pos + idx
			}
		}
		pos += idx + len(needle)
	}
}

func parseTagAttrs(raw []byte) (string, map[string]string) {
	s := strings.TrimSpace(string(raw))
	attrs := map[string]string{}
	if s == "" {
		return "", attrs
	}
	i := 0
	for i < len(s) && !isSpace(s[i]) {
		i++
	}
	tag := strings.ToLower(s[:i])
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		kStart := i
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' {
			i++
		}
		key := strings.ToLower(s[kStart:i])
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			if key != "" {
				attrs[key] = "true"
			}
			continue
		}
		i++ // skip '='
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			attrs[key] = ""
			break
		}
		if s[i] == '"' || s[i] == '\'' {
			q := s[i]
			i++
			vStart := i
			for i < len(s) && s[i] != q {
				i++
			}
			attrs[key] = s[vStart:i]
			if i < len(s) {
				i++
			}
		} else {
			vStart := i
			for i < len(s) && !isSpace(s[i]) {
				i++
			}
			attrs[key] = s[vStart:i]
		}
	}
	return tag, attrs
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func inspectJSONLD(raw []byte) (bool, *watch.Error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, nil
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return false, &watch.Error{Code: watch.CodeParse, Message: "the JSON-LD metadata block is malformed"}
	}
	return jsonLDNotFree(v), nil
}

func jsonLDNotFree(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if raw, ok := x["isAccessibleForFree"]; ok {
			switch b := raw.(type) {
			case bool:
				if !b {
					return true
				}
			case string:
				if strings.EqualFold(strings.TrimSpace(b), "false") {
					return true
				}
			}
		}
		for _, child := range x {
			if jsonLDNotFree(child) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if jsonLDNotFree(item) {
				return true
			}
		}
	}
	return false
}
