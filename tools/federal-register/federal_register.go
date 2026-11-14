// Package federalregister is the Tidegauge sub-tool that watches Federal
// Register notices and rulemakings via JSON API responses and XML feeds for
// FAR, DFARS and small-business rule updates.
//
// Reports carry document numbers, document types, action categories, CFR
// references, RINs, dates and dollar thresholds, and never copy raw prose or
// markup from the source.
package federalregister

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Name is this sub-tool's name in component.json and tools/registry.yaml.
const Name = "federal-register"

// Detection methods.
const (
	MethodETag     = "etag"
	MethodJSONDiff = "json-diff"
	MethodXMLDiff  = "xml-diff"
)

// Change kinds.
const (
	KindAdded    = "added"
	KindRemoved  = "removed"
	KindModified = "modified"
)

var maxSourceBytes = 8 << 20

// Snapshot is one saved version of a Federal Register JSON or XML source.
type Snapshot struct {
	ETag string
	Body io.Reader
}

// Report is the deterministic output of one Federal Register comparison.
type Report struct {
	Tool    string       `json:"tool"`
	Status  string       `json:"status"`
	Method  string       `json:"method,omitempty"`
	Changes []Change     `json:"changes,omitempty"`
	Error   *watch.Error `json:"error,omitempty"`
}

// Change describes one added, removed or modified Federal Register document.
type Change struct {
	DocNumber      string   `json:"doc_number"`
	Kind           string   `json:"kind"`
	DocType        string   `json:"doc_type"`
	ActionKind     string   `json:"action_kind"`
	CFR            []string `json:"cfr,omitempty"`
	RIN            []string `json:"rin,omitempty"`
	EffectiveOn    string   `json:"effective_on,omitempty"`
	Repealed       bool     `json:"repealed,omitempty"`
	AmountsRemoved []string `json:"amounts_removed,omitempty"`
	AmountsAdded   []string `json:"amounts_added,omitempty"`
	DatesRemoved   []string `json:"dates_removed,omitempty"`
	DatesAdded     []string `json:"dates_added,omitempty"`
}

// Summary is a compact, source-text-free baseline fingerprint for the worker.
type Summary struct {
	Format   string `json:"format"`
	DocCount int    `json:"doc_count"`
	Digest   string `json:"digest"`
	Repealed bool   `json:"repealed,omitempty"`
}

type docRecord struct {
	docNumber   string
	seq         int
	docType     string
	actionKind  string
	cfr         []string
	rin         []string
	published   string
	effectiveOn string
	repealed    bool
	sum         [32]byte
	amounts     []string
	dates       []string
}

func (d *docRecord) computeSum(textParts ...string) {
	h := sha256.New()
	writePart := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	writePart(d.docNumber)
	writePart(d.docType)
	writePart(d.actionKind)
	writePart(d.published)
	writePart(d.effectiveOn)
	for _, c := range d.cfr {
		writePart("cfr:" + c)
	}
	for _, r := range d.rin {
		writePart("rin:" + r)
	}
	for _, p := range textParts {
		writePart("txt:" + watch.Normalise(p))
	}
	h.Sum(d.sum[:0])
}

// Compare checks a new Federal Register snapshot against an earlier one.
func Compare(old, new Snapshot) Report {
	if watch.SameETag(old.ETag, new.ETag) {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: MethodETag}
	}
	oldDocs, oldFmt, werr := parseSnapshot(old.Body)
	if werr != nil {
		return failed(werr)
	}
	newDocs, newFmt, werr := parseSnapshot(new.Body)
	if werr != nil {
		return failed(werr)
	}
	if oldFmt != newFmt {
		return failed(&watch.Error{
			Code:    watch.CodeNeedsSubtool,
			Message: "the two versions do not share the same Federal Register format",
		})
	}
	method := MethodJSONDiff
	if newFmt == "xml" {
		method = MethodXMLDiff
	}

	oldByID := make(map[string]*docRecord, len(oldDocs))
	for _, d := range oldDocs {
		oldByID[d.docNumber] = d
	}
	seen := make(map[string]bool, len(oldDocs))
	var modifiedOrRemoved []*docRecord
	var added []*docRecord
	newByID := make(map[string]*docRecord, len(newDocs))
	for _, d := range newDocs {
		newByID[d.docNumber] = d
		prev, ok := oldByID[d.docNumber]
		if !ok {
			added = append(added, d)
			continue
		}
		seen[d.docNumber] = true
		if prev.sum != d.sum {
			modifiedOrRemoved = append(modifiedOrRemoved, prev)
		}
	}
	for _, d := range oldDocs {
		if !seen[d.docNumber] {
			modifiedOrRemoved = append(modifiedOrRemoved, d)
		}
	}
	if len(modifiedOrRemoved) == 0 && len(added) == 0 {
		return Report{Tool: Name, Status: watch.StatusUnchanged, Method: method}
	}
	sort.Slice(modifiedOrRemoved, func(i, j int) bool { return modifiedOrRemoved[i].seq < modifiedOrRemoved[j].seq })
	sort.Slice(added, func(i, j int) bool { return added[i].seq < added[j].seq })

	changes := make([]Change, 0, len(modifiedOrRemoved)+len(added))
	for _, prev := range modifiedOrRemoved {
		cur, ok := newByID[prev.docNumber]
		if !ok {
			changes = append(changes, Change{
				DocNumber:      prev.docNumber,
				Kind:           KindRemoved,
				DocType:        prev.docType,
				ActionKind:     prev.actionKind,
				CFR:            prev.cfr,
				RIN:            prev.rin,
				EffectiveOn:    prev.effectiveOn,
				Repealed:       prev.repealed,
				AmountsRemoved: watch.Sorted(prev.amounts),
				DatesRemoved:   watch.Sorted(prev.dates),
			})
			continue
		}
		amRm, amAdd := watch.MultisetDiff(prev.amounts, cur.amounts)
		dtRm, dtAdd := watch.MultisetDiff(prev.dates, cur.dates)
		changes = append(changes, Change{
			DocNumber:      cur.docNumber,
			Kind:           KindModified,
			DocType:        cur.docType,
			ActionKind:     cur.actionKind,
			CFR:            cur.cfr,
			RIN:            cur.rin,
			EffectiveOn:    cur.effectiveOn,
			Repealed:       cur.repealed,
			AmountsRemoved: amRm,
			AmountsAdded:   amAdd,
			DatesRemoved:   dtRm,
			DatesAdded:     dtAdd,
		})
	}
	for _, cur := range added {
		changes = append(changes, Change{
			DocNumber:    cur.docNumber,
			Kind:         KindAdded,
			DocType:      cur.docType,
			ActionKind:   cur.actionKind,
			CFR:          cur.cfr,
			RIN:          cur.rin,
			EffectiveOn:  cur.effectiveOn,
			Repealed:     cur.repealed,
			AmountsAdded: watch.Sorted(cur.amounts),
			DatesAdded:   watch.Sorted(cur.dates),
		})
	}
	return Report{Tool: Name, Status: watch.StatusChanged, Method: method, Changes: changes}
}

// Inspect validates one Federal Register feed or document and returns its
// compact canonical Summary.
func Inspect(r io.Reader) (Summary, *watch.Error) {
	docs, format, werr := parseSnapshot(r)
	if werr != nil {
		return Summary{}, werr
	}
	sorted := append([]*docRecord(nil), docs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].docNumber < sorted[j].docNumber })
	h := sha256.New()
	repealed := false
	for _, d := range sorted {
		h.Write([]byte(d.docNumber))
		h.Write([]byte{0})
		h.Write(d.sum[:])
		if d.repealed {
			repealed = true
		}
	}
	return Summary{
		Format:   format,
		DocCount: len(sorted),
		Digest:   hex.EncodeToString(h.Sum(nil)),
		Repealed: repealed,
	}, nil
}

func failed(err *watch.Error) Report {
	return Report{Tool: Name, Status: watch.StatusFor(err), Error: err}
}

func parseSnapshot(r io.Reader) ([]*docRecord, string, *watch.Error) {
	data, werr := watch.ReadBounded(r, maxSourceBytes)
	if werr != nil {
		return nil, "", werr
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is empty"}
	}
	switch trimmed[0] {
	case '{', '[':
		docs, err := parseJSON(trimmed)
		if err != nil {
			return nil, "", err
		}
		return docs, "json", nil
	case '<':
		docs, err := parseXML(trimmed)
		if err != nil {
			return nil, "", err
		}
		return docs, "xml", nil
	default:
		return nil, "", &watch.Error{Code: watch.CodeParse, Message: "the source is neither JSON nor XML"}
	}
}

type jsonCFRRef struct {
	Title int    `json:"title"`
	Part  int    `json:"part"`
	Sub   string `json:"subpart"`
}

type jsonDoc struct {
	DocumentNumber      string       `json:"document_number"`
	Type                string       `json:"type"`
	Title               string       `json:"title"`
	Abstract            string       `json:"abstract"`
	Action              string       `json:"action"`
	Dates               string       `json:"dates"`
	PublicationDate     string       `json:"publication_date"`
	EffectiveOn         string       `json:"effective_on"`
	RegulationIDNumber  string       `json:"regulation_id_number"`
	RegulationIDNumbers []string     `json:"regulation_id_numbers"`
	CFRReferences       []jsonCFRRef `json:"cfr_references"`
	DocketIDs           []string     `json:"docket_ids"`
	Significant         *bool        `json:"significant"`
	BodyHTMLURL         string       `json:"body_html_url"`
	HTMLURL             string       `json:"html_url"`
}

func parseJSON(data []byte) ([]*docRecord, *watch.Error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		var probe any
		if json.Unmarshal(data, &probe) == nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the JSON source is not a Federal Register object"}
		}
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the source is not well-formed JSON"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the source has trailing content after JSON"}
	}

	var list []jsonDoc
	switch {
	case raw["results"] != nil:
		if err := json.Unmarshal(raw["results"], &list); err != nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the Federal Register results list has an unfamiliar shape"}
		}
	case raw["document_number"] != nil:
		var single jsonDoc
		if err := json.Unmarshal(data, &single); err != nil {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the Federal Register document has an unfamiliar shape"}
		}
		list = []jsonDoc{single}
	default:
		return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the JSON object is not a Federal Register response"}
	}

	seen := make(map[string]bool, len(list))
	out := make([]*docRecord, 0, len(list))
	for i, jd := range list {
		id := watch.Normalise(jd.DocumentNumber)
		if id == "" {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a Federal Register entry has no document_number"}
		}
		if seen[id] {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "two Federal Register entries share a document_number"}
		}
		seen[id] = true
		kind, ok := mapDocType(jd.Type)
		if !ok {
			return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a Federal Register entry has an unknown document type"}
		}
		var cfrs []string
		for _, c := range jd.CFRReferences {
			if c.Title <= 0 {
				continue
			}
			if c.Part > 0 {
				cfrs = append(cfrs, fmt.Sprintf("%d CFR %d", c.Title, c.Part))
			} else {
				cfrs = append(cfrs, fmt.Sprintf("%d CFR", c.Title))
			}
		}
		cfrs = append(cfrs, extractCFR(jd.Title+" "+jd.Abstract)...)
		var rins []string
		if jd.RegulationIDNumber != "" {
			rins = append(rins, extractRINs(jd.RegulationIDNumber)...)
		}
		for _, r := range jd.RegulationIDNumbers {
			rins = append(rins, extractRINs(r)...)
		}
		actionKind := classifyAction(jd.Action, kind)
		combined := strings.Join([]string{jd.Title, jd.Action, jd.Abstract, jd.Dates, jd.PublicationDate, jd.EffectiveOn}, " ")
		rec := &docRecord{
			docNumber:   id,
			seq:         i,
			docType:     kind,
			actionKind:  actionKind,
			cfr:         watch.SortedUnique(cfrs),
			rin:         watch.SortedUnique(rins),
			published:   watch.Normalise(jd.PublicationDate),
			effectiveOn: watch.Normalise(jd.EffectiveOn),
			repealed:    actionKind == "repeal",
			amounts:     watch.Amounts(combined),
			dates:       watch.Dates(combined),
		}
		rec.computeSum(jd.Title, jd.Action, jd.Abstract, jd.Dates, strings.Join(watch.SortedUnique(jd.DocketIDs), ","))
		out = append(out, rec)
	}
	return out, nil
}

func parseXML(data []byte) ([]*docRecord, *watch.Error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		rootSeen     bool
		rootDone     bool
		rootName     string
		depth        int
		issuePubDate string
		inDoc        bool
		docDepth     int
		fieldTag     string
		fieldDepth   int
		fieldText    strings.Builder
		inIssueDate  bool
		issueDateTxt strings.Builder
		curDocType   string
		curDocNum    string
		curAction    string
		curEffDate   string
		curCFRs      []string
		curRINs      []string
		curBody      strings.Builder
		docs         []*docRecord
		seen         = map[string]bool{}
	)

	isFieldTag := func(local string) bool {
		switch local {
		case "FRDOC", "CFR", "RIN", "ACTION", "EFFDATE":
			return true
		}
		return false
	}

	startDoc := func(el xml.StartElement) bool {
		kind, ok := mapDocType(el.Name.Local)
		if !ok || el.Name.Space != "" {
			return false
		}
		inDoc = true
		docDepth = depth
		curDocType = kind
		curDocNum, curAction, curEffDate = "", "", ""
		curCFRs, curRINs = nil, nil
		curBody.Reset()
		fieldTag = ""
		for _, a := range el.Attr {
			if a.Name.Local == "DOCNO" {
				curDocNum = watch.Normalise(a.Value)
			}
		}
		return true
	}

	finishDoc := func() *watch.Error {
		id := watch.Normalise(curDocNum)
		if id == "" {
			return &watch.Error{Code: watch.CodeNeedsSubtool, Message: "a Federal Register XML notice has no document number"}
		}
		if seen[id] {
			return &watch.Error{Code: watch.CodeNeedsSubtool, Message: "two Federal Register XML notices share a document number"}
		}
		seen[id] = true
		actionKind := classifyAction(curAction, curDocType)
		bodyText := curBody.String()
		effDates := watch.Dates(curEffDate)
		eff := ""
		if len(effDates) > 0 {
			eff = effDates[0]
		}
		rec := &docRecord{
			docNumber:   id,
			seq:         len(docs),
			docType:     curDocType,
			actionKind:  actionKind,
			cfr:         watch.SortedUnique(curCFRs),
			rin:         watch.SortedUnique(curRINs),
			published:   issuePubDate,
			effectiveOn: eff,
			repealed:    actionKind == "repeal",
			amounts:     watch.Amounts(bodyText),
			dates:       watch.Dates(bodyText),
		}
		rec.computeSum(bodyText)
		docs = append(docs, rec)
		inDoc = false
		return nil
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &watch.Error{Code: watch.CodeParse, Message: "the source is not well-formed XML"}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if rootDone {
				return nil, &watch.Error{Code: watch.CodeParse, Message: "the source has content after its root element"}
			}
			depth++
			if !rootSeen {
				rootSeen = true
				rootName = t.Name.Local
				if t.Name.Space != "" {
					return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the XML source is not a Federal Register document"}
				}
				if rootName == "FEDREG" {
					continue
				}
				if !startDoc(t) {
					return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the XML source is not a Federal Register document"}
				}
				continue
			}
			if !inDoc {
				if rootName == "FEDREG" && depth == 2 {
					if startDoc(t) {
						continue
					}
					if t.Name.Local == "DATE" {
						inIssueDate = true
						issueDateTxt.Reset()
						continue
					}
					return nil, &watch.Error{Code: watch.CodeNeedsSubtool, Message: "the FEDREG document holds an unfamiliar section"}
				}
				continue
			}
			curBody.WriteByte(' ')
			if fieldTag == "" && t.Name.Space == "" && isFieldTag(t.Name.Local) {
				fieldTag = t.Name.Local
				fieldDepth = depth
				fieldText.Reset()
			}
		case xml.EndElement:
			if inDoc && depth == docDepth {
				if werr := finishDoc(); werr != nil {
					return nil, werr
				}
			} else if inDoc {
				curBody.WriteByte(' ')
				if fieldTag != "" && depth == fieldDepth {
					val := watch.Normalise(fieldText.String())
					switch fieldTag {
					case "FRDOC":
						if n := extractDocNum(val); n != "" {
							curDocNum = n
						} else if curDocNum == "" {
							curDocNum = val
						}
					case "CFR":
						curCFRs = append(curCFRs, extractCFR(val)...)
					case "RIN":
						curRINs = append(curRINs, extractRINs(val)...)
					case "ACTION":
						curAction = val
					case "EFFDATE":
						curEffDate = val
					}
					fieldTag = ""
					fieldText.Reset()
				}
			} else if inIssueDate && depth == 2 && t.Name.Local == "DATE" {
				if ds := watch.Dates(issueDateTxt.String()); len(ds) > 0 {
					issuePubDate = ds[0]
				}
				inIssueDate = false
				issueDateTxt.Reset()
			}
			depth--
			if depth == 0 && rootSeen {
				rootDone = true
			}
		case xml.CharData:
			if !rootSeen && strings.TrimSpace(string(t)) != "" {
				return nil, &watch.Error{Code: watch.CodeParse, Message: "the source has text outside its root element"}
			}
			if inDoc {
				curBody.Write(t)
				if fieldTag != "" {
					fieldText.Write(t)
				}
			} else if inIssueDate {
				issueDateTxt.Write(t)
			}
		}
	}
	if !rootDone {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "the source is empty or truncated"}
	}
	return docs, nil
}
