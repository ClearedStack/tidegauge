// Package ecfr is the Tidegauge sub-tool that watches the Electronic Code
// of Federal Regulations. Given two saved versions of one eCFR source it
// says whether the source changed and where.
//
// It is deterministic: no clock, no randomness, no network, no model. The
// report never carries text from the source apart from division numbers,
// paragraph markers, dollar amounts and dates.
package ecfr

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Name is this sub-tool's name in component.json and tools/registry.yaml.
const Name = "ecfr"

// How a result was reached.
const (
	MethodETag    = "etag"     // the two versions carry the same HTTP ETag
	MethodXMLDiff = "xml-diff" // both versions were parsed and compared
)

// Kinds of change, also used for paragraphs.
const (
	KindAdded    = "added"
	KindRemoved  = "removed"
	KindModified = "modified"
)

// What changed in a modified division.
const (
	FieldHeading = "heading"
	FieldText    = "text"
)

// Snapshot is one saved version of a source: the response body and, when
// the server sent one, its ETag header value.
//
// Body is read from its current position and may be read twice: once to
// find which divisions differ and, if any do, once more to describe them.
type Snapshot struct {
	ETag string
	Body io.ReadSeeker
}

// Report is the result of one check.
type Report struct {
	Tool    string       `json:"tool"`
	Status  string       `json:"status"`
	Method  string       `json:"method,omitempty"`
	Changes []Change     `json:"changes,omitempty"`
	Error   *watch.Error `json:"error,omitempty"`
}

// Change is one division that was added, removed or modified.
type Change struct {
	// Where is the path from the document's root down to the division,
	// each step "TYPE N", for example ["PART 901", "SECTION 901.102"].
	Where []string `json:"where"`
	Kind  string   `json:"kind"`
	// Fields lists what changed in a modified division: "heading", "text".
	Fields []string `json:"fields,omitempty"`
	// Paragraphs locates the change inside the division's own text.
	Paragraphs []ParagraphChange `json:"paragraphs,omitempty"`
	// Dollar amounts, as written, that left or entered the division.
	AmountsRemoved []string `json:"amounts_removed,omitempty"`
	AmountsAdded   []string `json:"amounts_added,omitempty"`
	// Dates (YYYY-MM-DD) that left or entered the division.
	DatesRemoved []string `json:"dates_removed,omitempty"`
	DatesAdded   []string `json:"dates_added,omitempty"`
}

// ParagraphChange is one paragraph-level element removed from the old
// version or added in the new one. Position counts from 1 within the
// division: in the old version for "removed", in the new for "added".
// Marker is the paragraph's leading marker, such as "(b)", when it has one.
type ParagraphChange struct {
	Op       string `json:"op"`
	Position int    `json:"position"`
	Marker   string `json:"marker,omitempty"`
}

// Compare checks a new version of a source against an old one.
//
// If both carry an ETag and the two match, the source is unchanged and
// neither body is read; this is the saved-file form of a conditional
// request answered "304 Not Modified". Otherwise both bodies are parsed and
// compared division by division.
//
// Removed and modified divisions are reported first, in the old version's
// order, then added ones in the new version's order.
func Compare(old, new Snapshot) Report {
	if sameETag(old.ETag, new.ETag) {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: MethodETag}
	}
	oldStart, err := old.Body.Seek(0, io.SeekCurrent)
	if err != nil {
		return failed(unreadable())
	}
	newStart, err := new.Body.Seek(0, io.SeekCurrent)
	if err != nil {
		return failed(unreadable())
	}

	// First reading: keep one short fingerprint per division of the old
	// version, then stream the new version past it.
	type entry struct {
		sum  [16]byte
		seq  int
		seen bool
	}
	index := map[string]*entry{}
	if werr := parse(old.Body, nil, func(u *unit) {
		index[u.key] = &entry{sum: u.fingerprint(), seq: u.seq}
	}); werr != nil {
		return failed(werr)
	}
	type ref struct {
		key string
		seq int
	}
	var before, added []ref // removed or modified; added
	if werr := parse(new.Body, nil, func(u *unit) {
		e, ok := index[u.key]
		switch {
		case !ok:
			added = append(added, ref{u.key, u.seq})
		case e.sum != u.fingerprint():
			e.seen = true
			before = append(before, ref{u.key, e.seq})
		default:
			e.seen = true
		}
	}); werr != nil {
		return failed(werr)
	}
	for key, e := range index {
		if !e.seen {
			before = append(before, ref{key, e.seq})
		}
	}
	index = nil
	if len(before) == 0 && len(added) == 0 {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: MethodXMLDiff}
	}
	sort.Slice(before, func(i, j int) bool { return before[i].seq < before[j].seq })
	sort.Slice(added, func(i, j int) bool { return added[i].seq < added[j].seq })

	// Second reading: collect the detail of only the divisions that differ.
	wanted := make(map[string]bool, len(before)+len(added))
	for _, r := range before {
		wanted[r.key] = true
	}
	for _, r := range added {
		wanted[r.key] = true
	}
	want := func(key string) bool { return wanted[key] }
	if _, err := old.Body.Seek(oldStart, io.SeekStart); err != nil {
		return failed(unreadable())
	}
	if _, err := new.Body.Seek(newStart, io.SeekStart); err != nil {
		return failed(unreadable())
	}
	oldUnits, newUnits := map[string]*unit{}, map[string]*unit{}
	collect := func(into map[string]*unit) func(*unit) {
		return func(u *unit) {
			if u.detail {
				into[u.key] = u
			}
		}
	}
	if werr := parse(old.Body, want, collect(oldUnits)); werr != nil {
		return failed(werr)
	}
	if werr := parse(new.Body, want, collect(newUnits)); werr != nil {
		return failed(werr)
	}
	changes := make([]Change, 0, len(before)+len(added))
	for _, r := range before {
		changes = append(changes, change(oldUnits[r.key], newUnits[r.key]))
	}
	for _, r := range added {
		changes = append(changes, change(nil, newUnits[r.key]))
	}
	return Report{Tool: Name, Status: watch.StatusChanged, Method: MethodXMLDiff, Changes: changes}
}

func unreadable() *watch.Error {
	return &watch.Error{Code: watch.CodeParse, Message: "a saved version could not be read"}
}

func failed(err *watch.Error) Report {
	return Report{Tool: Name, Status: watch.StatusFor(err), Error: err}
}

// sameETag reports whether two ETag header values name the same version.
// Both must be present and well formed. A leading "W/" (a weak validator)
// is ignored, as it is when a server answers a conditional GET.
func sameETag(a, b string) bool {
	x, ok := etagOpaque(a)
	if !ok {
		return false
	}
	y, ok := etagOpaque(b)
	return ok && x == y
}

func etagOpaque(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' || strings.ContainsAny(s[1:len(s)-1], "\" \t\r\n") {
		return "", false
	}
	return s, true
}

// Summary is a compact, source-text-free fingerprint of one eCFR document used
// by the continuous worker to persist baselines inside Muster's 64 KiB result
// limit.
type Summary struct {
	Root     string `json:"root"`
	DivCount int    `json:"div_count"`
	Digest   string `json:"digest"`
	Reserved bool   `json:"reserved,omitempty"`
}

// Inspect streams one eCFR XML document and returns its canonical Summary,
// ignoring formatting and division order just as Compare does.
func Inspect(r io.Reader) (Summary, *watch.Error) {
	if r == nil {
		return Summary{}, unreadable()
	}
	type item struct {
		key  string
		head [32]byte
		body [32]byte
	}
	var items []item
	var rootKey string
	var rootReserved bool
	var rootBody [32]byte
	emptySum := sha256.Sum256(nil)
	if werr := parse(r, nil, func(u *unit) {
		items = append(items, item{key: u.key, head: u.head, body: u.body})
		if !strings.Contains(u.key, keySep) {
			rootKey = u.key
			rootReserved = u.reserved
			rootBody = u.body
		}
	}); werr != nil {
		return Summary{}, werr
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	h := sha256.New()
	for _, it := range items {
		h.Write([]byte(it.key))
		h.Write([]byte{0})
		h.Write(it.head[:])
		h.Write(it.body[:])
	}
	reserved := len(items) == 1 && rootReserved && rootBody == emptySum
	return Summary{
		Root:     rootKey,
		DivCount: len(items),
		Digest:   hex.EncodeToString(h.Sum(nil)),
		Reserved: reserved,
	}, nil
}
