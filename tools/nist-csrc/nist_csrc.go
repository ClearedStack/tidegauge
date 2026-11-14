// Package nistcsrc is the Tidegauge sub-tool that watches the NIST Computer
// Security Resource Center (CSRC) via Atom/RSS feeds and publication HTML DOM
// pages for updates to cybersecurity standards.
//
// Reports carry only canonical publication identifiers, series codes, revision
// strings, lifecycle statuses, superseding identifiers and ISO dates. No prose
// or markup from the source is ever copied into a report.
package nistcsrc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Name is this sub-tool's name in component.json and tools/registry.yaml.
const Name = "nist-csrc"

// Detection methods.
const (
	MethodETag     = "etag"
	MethodFeedDiff = "feed-diff"
	MethodDOMDiff  = "dom-diff"
)

// Change kinds.
const (
	KindAdded    = "added"
	KindRemoved  = "removed"
	KindModified = "modified"
)

const atomNS = "http://www.w3.org/2005/Atom"

var maxSourceBytes = 8 << 20

// Snapshot is one saved version of a NIST CSRC feed or publication page.
type Snapshot struct {
	ETag string
	Body io.Reader
}

// Report is the deterministic output of one NIST CSRC comparison.
type Report struct {
	Tool    string       `json:"tool"`
	Status  string       `json:"status"`
	Method  string       `json:"method,omitempty"`
	Changes []Change     `json:"changes,omitempty"`
	Error   *watch.Error `json:"error,omitempty"`
}

// Change describes one added, removed or modified NIST publication entry.
type Change struct {
	PubID        string   `json:"pub_id"`
	Kind         string   `json:"kind"`
	Series       string   `json:"series"`
	Revision     string   `json:"revision,omitempty"`
	Status       string   `json:"status"`
	SupersededBy []string `json:"superseded_by,omitempty"`
	Repealed     bool     `json:"repealed,omitempty"`
	DatesRemoved []string `json:"dates_removed,omitempty"`
	DatesAdded   []string `json:"dates_added,omitempty"`
}

// Summary is a compact, source-text-free baseline fingerprint for the worker.
type Summary struct {
	Format   string `json:"format"`
	PubCount int    `json:"pub_count"`
	Digest   string `json:"digest"`
	Repealed bool   `json:"repealed,omitempty"`
}

type pubRecord struct {
	pubID        string
	seq          int
	series       string
	revision     string
	status       string
	supersededBy []string
	repealed     bool
	dates        []string
	sum          [32]byte
}

func (p *pubRecord) computeSum(extra ...string) {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(p.pubID)
	write(p.series)
	write(p.revision)
	write(p.status)
	for _, s := range p.supersededBy {
		write("sup:" + s)
	}
	for _, d := range p.dates {
		write("date:" + d)
	}
	for _, e := range extra {
		write("txt:" + watch.Normalise(e))
	}
	h.Sum(p.sum[:0])
}

// Compare checks a new NIST CSRC snapshot against an earlier one.
func Compare(old, new Snapshot) Report {
	if watch.SameETag(old.ETag, new.ETag) {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: MethodETag}
	}
	oldPubs, oldFmt, werr := parseSnapshot(old.Body)
	if werr != nil {
		return failed(werr)
	}
	newPubs, newFmt, werr := parseSnapshot(new.Body)
	if werr != nil {
		return failed(werr)
	}
	if oldFmt != newFmt {
		return failed(&watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the two versions do not share the same NIST CSRC format",
		})
	}
	method := MethodFeedDiff
	if newFmt == "dom" {
		method = MethodDOMDiff
	}

	oldByID := make(map[string]*pubRecord, len(oldPubs))
	for _, p := range oldPubs {
		oldByID[p.pubID] = p
	}
	seen := make(map[string]bool, len(oldPubs))
	newByID := make(map[string]*pubRecord, len(newPubs))
	var modifiedOrRemoved []*pubRecord
	var added []*pubRecord
	for _, p := range newPubs {
		newByID[p.pubID] = p
		prev, ok := oldByID[p.pubID]
		if !ok {
			added = append(added, p)
			continue
		}
		seen[p.pubID] = true
		if prev.sum != p.sum {
			modifiedOrRemoved = append(modifiedOrRemoved, prev)
		}
	}
	for _, p := range oldPubs {
		if !seen[p.pubID] {
			modifiedOrRemoved = append(modifiedOrRemoved, p)
		}
	}
	if len(modifiedOrRemoved) == 0 && len(added) == 0 {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: method}
	}
	sort.Slice(modifiedOrRemoved, func(i, j int) bool { return modifiedOrRemoved[i].seq < modifiedOrRemoved[j].seq })
	sort.Slice(added, func(i, j int) bool { return added[i].seq < added[j].seq })

	changes := make([]Change, 0, len(modifiedOrRemoved)+len(added))
	for _, prev := range modifiedOrRemoved {
		cur, ok := newByID[prev.pubID]
		if !ok {
			changes = append(changes, Change{
				PubID:        prev.pubID,
				Kind:         KindRemoved,
				Series:       prev.series,
				Revision:     prev.revision,
				Status:       prev.status,
				SupersededBy: prev.supersededBy,
				Repealed:     prev.repealed,
				DatesRemoved: watch.SortedUnique(prev.dates),
			})
			continue
		}
		dtRm, dtAdd := watch.MultisetDiff(watch.SortedUnique(prev.dates), watch.SortedUnique(cur.dates))
		changes = append(changes, Change{
			PubID:        cur.pubID,
			Kind:         KindModified,
			Series:       cur.series,
			Revision:     cur.revision,
			Status:       cur.status,
			SupersededBy: cur.supersededBy,
			Repealed:     cur.repealed,
			DatesRemoved: dtRm,
			DatesAdded:   dtAdd,
		})
	}
	for _, cur := range added {
		changes = append(changes, Change{
			PubID:        cur.pubID,
			Kind:         KindAdded,
			Series:       cur.series,
			Revision:     cur.revision,
			Status:       cur.status,
			SupersededBy: cur.supersededBy,
			Repealed:     cur.repealed,
			DatesAdded:   watch.SortedUnique(cur.dates),
		})
	}
	return Report{Tool: Name, Status: watch.StatusChanged, Method: method, Changes: changes}
}

// Inspect validates one NIST CSRC feed or DOM snapshot and returns its Summary.
func Inspect(r io.Reader) (Summary, *watch.Error) {
	pubs, format, werr := parseSnapshot(r)
	if werr != nil {
		return Summary{}, werr
	}
	sorted := append([]*pubRecord(nil), pubs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].pubID < sorted[j].pubID })
	h := sha256.New()
	repealed := false
	for _, p := range sorted {
		h.Write([]byte(p.pubID))
		h.Write([]byte{0})
		h.Write(p.sum[:])
		if p.repealed {
			repealed = true
		}
	}
	// A single-publication DOM page is considered repealed when its sole
	// publication is withdrawn or superseded; multi-entry feeds track mixed
	// items, so Repealed is only asserted when all entries are repealed.
	if len(sorted) > 1 {
		repealed = true
		for _, p := range sorted {
			if !p.repealed {
				repealed = false
				break
			}
		}
	}
	return Summary{
		Format:   format,
		PubCount: len(sorted),
		Digest:   hex.EncodeToString(h.Sum(nil)),
		Repealed: repealed,
	}, nil
}

func failed(err *watch.Error) Report {
	return Report{Tool: Name, Status: watch.StatusFor(err), Error: err}
}

func parseSnapshot(r io.Reader) ([]*pubRecord, string, *watch.Error) {
	data, werr := watch.ReadBounded(r, maxSourceBytes)
	if werr != nil {
		return nil, "", werr
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is empty"}
	}
	if trimmed[0] != '<' {
		return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is neither XML nor HTML"}
	}
	if looksLikeHTML(trimmed) {
		pubs, err := parseDOM(trimmed)
		if err != nil {
			return nil, "", err
		}
		return pubs, "dom", nil
	}
	return parseFeed(trimmed)
}

func looksLikeHTML(trimmed []byte) bool {
	head := trimmed
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(string(head))
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Updated    string         `xml:"updated"`
	Published  string         `xml:"published"`
	Summary    string         `xml:"summary"`
	Content    string         `xml:"content"`
	Categories []atomCategory `xml:"category"`
}

type atomCategory struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr"`
}

type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	GUID        string   `xml:"guid"`
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	PubDate     string   `xml:"pubDate"`
	Categories  []string `xml:"category"`
	Description string   `xml:"description"`
}

func parseFeed(data []byte) ([]*pubRecord, string, *watch.Error) {
	// Inspect root element strictly first so malformed XML -> E_PARSE and
	// unfamiliar XML -> E_NEEDS_SUBTOOL.
	dec := xml.NewDecoder(bytes.NewReader(data))
	var root xml.StartElement
	foundRoot := false
	depth := 0
	rootClosed := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is not well-formed XML"}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if rootClosed {
				return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source has content after its root element"}
			}
			if !foundRoot {
				root = t
				foundRoot = true
			}
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 && foundRoot {
				rootClosed = true
			}
		case xml.CharData:
			if !foundRoot && strings.TrimSpace(string(t)) != "" {
				return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source has text outside its root element"}
			}
		}
	}
	if !rootClosed {
		return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is empty or truncated"}
	}

	switch {
	case root.Name.Local == "feed" && root.Name.Space == atomNS:
		var f atomFeed
		if err := xml.Unmarshal(data, &f); err != nil {
			return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the Atom feed could not be decoded"}
		}
		if len(f.Entries) == 0 {
			return nil, "", &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the Atom feed has no publication entries"}
		}
		pubs, werr := buildAtomRecords(f.Entries)
		if werr != nil {
			return nil, "", werr
		}
		return pubs, "atom", nil

	case root.Name.Local == "rss" && root.Name.Space == "":
		var r rssDoc
		if err := xml.Unmarshal(data, &r); err != nil {
			return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the RSS feed could not be decoded"}
		}
		if len(r.Channel.Items) == 0 {
			return nil, "", &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the RSS feed has no publication items"}
		}
		pubs, werr := buildRSSRecords(r.Channel.Items)
		if werr != nil {
			return nil, "", werr
		}
		return pubs, "rss", nil

	default:
		return nil, "", &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the XML source is not a NIST CSRC Atom or RSS feed"}
	}
}

func buildAtomRecords(entries []atomEntry) ([]*pubRecord, *watch.Error) {
	seen := make(map[string]bool, len(entries))
	out := make([]*pubRecord, 0, len(entries))
	for i, e := range entries {
		meta, ok := extractPubMeta(e.Title, e.ID, e.Summary)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "an Atom entry does not carry a recognizable NIST publication identifier"}
		}
		if seen[meta.pubID] {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "two Atom entries name the same publication identifier"}
		}
		seen[meta.pubID] = true

		explicitStatus := ""
		for _, c := range e.Categories {
			term := strings.TrimSpace(c.Term)
			if term == "" {
				term = strings.TrimSpace(c.Label)
			}
			if _, ok := statusAliases[strings.ToLower(term)]; ok {
				explicitStatus = term
				break
			}
		}
		status, ok := normalizeStatus(explicitStatus, e.Title+" "+e.Summary+" "+e.Content)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "an Atom entry has an unknown publication status"}
		}
		combined := strings.Join([]string{e.Title, e.Updated, e.Published, e.Summary, e.Content}, " ")
		sup := extractSuperseding(meta.pubID, e.Summary+" "+e.Content)
		dates := collectDates(combined, e.Updated, e.Published)
		rec := &pubRecord{
			pubID:        meta.pubID,
			seq:          i,
			series:       meta.series,
			revision:     meta.revision,
			status:       status,
			supersededBy: sup,
			repealed:     status == "withdrawn" || status == "superseded",
			dates:        dates,
		}
		rec.computeSum(e.Title, e.Summary, e.Content)
		out = append(out, rec)
	}
	return out, nil
}

func buildRSSRecords(items []rssItem) ([]*pubRecord, *watch.Error) {
	seen := make(map[string]bool, len(items))
	out := make([]*pubRecord, 0, len(items))
	for i, it := range items {
		meta, ok := extractPubMeta(it.Title, it.GUID, it.Description)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "an RSS item does not carry a recognizable NIST publication identifier"}
		}
		if seen[meta.pubID] {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "two RSS items name the same publication identifier"}
		}
		seen[meta.pubID] = true

		explicitStatus := ""
		for _, c := range it.Categories {
			if _, ok := statusAliases[strings.ToLower(strings.TrimSpace(c))]; ok {
				explicitStatus = c
				break
			}
		}
		status, ok := normalizeStatus(explicitStatus, it.Title+" "+it.Description)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "an RSS item has an unknown publication status"}
		}
		combined := strings.Join([]string{it.Title, it.PubDate, it.Description}, " ")
		sup := extractSuperseding(meta.pubID, it.Description)
		dates := collectDates(combined, it.PubDate)
		rec := &pubRecord{
			pubID:        meta.pubID,
			seq:          i,
			series:       meta.series,
			revision:     meta.revision,
			status:       status,
			supersededBy: sup,
			repealed:     status == "withdrawn" || status == "superseded",
			dates:        dates,
		}
		rec.computeSum(it.Title, it.Description)
		out = append(out, rec)
	}
	return out, nil
}

func parseDOM(data []byte) ([]*pubRecord, *watch.Error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var (
		seenHTML      bool
		closedHTML    bool
		depth         int
		metaPubID     string
		metaTitle     string
		metaStatus    string
		metaRevision  string
		metaPubDate   string
		metaUpdated   string
		metaSupersede []string
		csrcMarker    bool
		captureDepth  int
		captureField  string
		captureBuf    strings.Builder
		errataNotes   []string
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
					return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the HTML source does not start with an html element"}
				}
				seenHTML = true
			}
			depth++

			if tag == "meta" {
				name := strings.ToLower(attrVal(t.Attr, "name"))
				content := attrVal(t.Attr, "content")
				switch name {
				case "citation_technical_report_number", "csrc:pub-id":
					csrcMarker = true
					metaPubID = content
				case "citation_title":
					metaTitle = content
				case "citation_publication_date", "dcterms.issued":
					metaPubDate = content
				case "dcterms.modified", "csrc:updated":
					metaUpdated = content
				case "csrc:status":
					csrcMarker = true
					metaStatus = content
				case "csrc:revision":
					csrcMarker = true
					metaRevision = content
				case "csrc:superseded-by":
					csrcMarker = true
					metaSupersede = append(metaSupersede, extractAllPubIDs(content)...)
				}
			}

			if dp := attrVal(t.Attr, "data-csrc-pub"); dp != "" {
				csrcMarker = true
				if metaPubID == "" {
					metaPubID = dp
				}
			}
			if ds := attrVal(t.Attr, "data-pub-status"); ds != "" {
				csrcMarker = true
				metaStatus = ds
			}
			if dr := attrVal(t.Attr, "data-pub-revision"); dr != "" {
				csrcMarker = true
				metaRevision = dr
			}
			if dsup := attrVal(t.Attr, "data-superseded-by"); dsup != "" {
				csrcMarker = true
				metaSupersede = append(metaSupersede, extractAllPubIDs(dsup)...)
			}

			id := strings.ToLower(attrVal(t.Attr, "id"))
			if captureField == "" {
				switch id {
				case "pub-title", "pub-header-title":
					csrcMarker = true
					captureField = "title"
					captureDepth = depth
					captureBuf.Reset()
				case "pub-status", "pub-status-badge":
					csrcMarker = true
					captureField = "status"
					captureDepth = depth
					captureBuf.Reset()
				case "pub-release-date", "pub-date":
					csrcMarker = true
					captureField = "date"
					captureDepth = depth
					captureBuf.Reset()
				case "pub-errata", "pub-updates":
					csrcMarker = true
					captureField = "errata"
					captureDepth = depth
					captureBuf.Reset()
				case "pub-superseded-by":
					csrcMarker = true
					captureField = "superseded"
					captureDepth = depth
					captureBuf.Reset()
				}
			}

		case xml.EndElement:
			tag := strings.ToLower(t.Name.Local)
			if captureField != "" && depth == captureDepth {
				val := watch.Normalise(captureBuf.String())
				switch captureField {
				case "title":
					if metaTitle == "" {
						metaTitle = val
					}
				case "status":
					if metaStatus == "" {
						metaStatus = val
					}
				case "date":
					metaUpdated = metaUpdated + " " + val
				case "errata":
					errataNotes = append(errataNotes, val)
				case "superseded":
					metaSupersede = append(metaSupersede, extractAllPubIDs(val)...)
				}
				captureField = ""
				captureBuf.Reset()
			}
			depth--
			if tag == "html" && depth == 0 {
				closedHTML = true
			}

		case xml.CharData:
			if captureField != "" {
				captureBuf.Write(t)
			}
		}
	}

	if !seenHTML || !closedHTML {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the HTML source is incomplete or truncated"}
	}
	if !csrcMarker {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the HTML page does not match the known NIST CSRC publication layout"}
	}

	meta, ok := extractPubMeta(metaPubID, metaTitle)
	if !ok {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the NIST CSRC page has no recognizable publication identifier"}
	}
	if metaRevision != "" {
		if _, revOK := extractPubMeta(meta.pubID + " " + metaRevision); revOK {
			m2, _ := extractPubMeta(meta.pubID + " " + metaRevision)
			if m2.revision != "" {
				meta.revision = m2.revision
			}
		} else {
			meta.revision = strings.ToLower(watch.Normalise(metaRevision))
		}
	}

	status, ok := normalizeStatus(metaStatus, metaTitle)
	if !ok {
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the NIST CSRC page reports an unknown publication status"}
	}

	combinedDates := strings.Join(append([]string{metaPubDate, metaUpdated}, errataNotes...), " ")
	dates := collectDates(combinedDates, metaPubDate, metaUpdated)
	sup := filterOutSelf(meta.pubID, metaSupersede)

	rec := &pubRecord{
		pubID:        meta.pubID,
		seq:          0,
		series:       meta.series,
		revision:     meta.revision,
		status:       status,
		supersededBy: sup,
		repealed:     status == "withdrawn" || status == "superseded",
		dates:        dates,
	}
	rec.computeSum(metaTitle, strings.Join(errataNotes, " | "))
	return []*pubRecord{rec}, nil
}

func extractSuperseding(selfID string, text string) []string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "superseded by")
	if idx < 0 {
		idx = strings.Index(lower, "replaced by")
	}
	if idx < 0 {
		return nil
	}
	return filterOutSelf(selfID, extractAllPubIDs(text[idx:]))
}

func filterOutSelf(selfID string, ids []string) []string {
	var filtered []string
	for _, id := range ids {
		if !strings.EqualFold(id, selfID) {
			filtered = append(filtered, id)
		}
	}
	return watch.SortedUnique(filtered)
}

func collectDates(combined string, rawStamps ...string) []string {
	dates := watch.Dates(combined)
	for _, raw := range rawStamps {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC3339,
			time.RFC1123Z,
			time.RFC1123,
			"Mon, 2 Jan 2006 15:04:05 MST",
			"Mon, 2 Jan 2006 15:04:05 -0700",
			"2006/01/02",
		} {
			if t, err := time.Parse(layout, s); err == nil {
				dates = append(dates, t.UTC().Format("2006-01-02"))
				break
			}
		}
	}
	return watch.SortedUnique(dates)
}
