package ecfr

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// maxBlockBytes caps the text of one paragraph-level element. The document
// is read as a stream and only one such element is held at a time, so this
// is what bounds memory per element. It is a variable so a test can lower it.
var maxBlockBytes = 1 << 20

// unit is one division of the document (a part, a subpart, a section ...)
// reduced to what the comparison needs. The source text itself is not kept.
type unit struct {
	key      string   // labels from the root down, joined by keySep
	seq      int      // position in the document, counting divisions from 0
	head     [32]byte // digest of the heading
	body     [32]byte // digest of the division's own text, excluding sub-divisions
	reserved bool     // heading marks this division [Reserved]

	// Filled only for divisions the caller asked for in detail.
	detail  bool
	blocks  []block  // one per paragraph-level element, in order
	amounts []string // dollar amounts found in the heading and text
	dates   []string // calendar dates found in the heading and text, as YYYY-MM-DD
}

// keySep joins labels into a key. A label cannot hold it: an XML document
// cannot contain a zero byte.
const keySep = "\x00"

// where splits a key back into its labels.
func where(key string) []string { return strings.Split(key, keySep) }

// fingerprint is a short digest of everything compared in a division.
func (u *unit) fingerprint() [16]byte {
	sum := sha256.Sum256(append(u.head[:], u.body[:]...))
	return [16]byte(sum[:16])
}

// block is one paragraph-level element: enough to tell whether it is the
// same paragraph as another, and its leading marker such as "(b)" if any.
type block struct {
	sum    uint64
	marker string
}

type frame struct {
	u        *unit
	h        hash.Hash
	children map[string]bool
	headSeen bool
	loose    strings.Builder // text sitting directly in the division
}

func parseError(msg string) *watch.Error {
	return &watch.Error{Code: watch.CodeParse, Message: msg}
}

func needsSubtool(msg string) *watch.Error {
	return &watch.Error{Code: watch.CodeNeedsSubtool, Message: msg}
}

// parse reads one eCFR XML document as a stream and calls emit once for
// each division, when the division closes. A division for which wantDetail
// returns true also gets its paragraphs, amounts and dates; wantDetail may
// be nil. Nothing is kept after emit returns, so memory does not grow with
// the size of the document. An error never carries text from the document.
func parse(r io.Reader, wantDetail func(key string) bool, emit func(*unit)) *watch.Error {
	dec := xml.NewDecoder(r)
	var (
		seq        int
		stack      []*frame
		rootDone   bool
		inBlock    bool
		blockDepth int
		blockHead  bool
		text       strings.Builder
	)
	emptyHead := sha256.Sum256(nil)

	addBlock := func(f *frame, s string) {
		if s == "" {
			return
		}
		sum := sha256.Sum256([]byte(s))
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		f.h.Write(n[:])
		f.h.Write([]byte(s))
		if f.u.detail {
			f.u.blocks = append(f.u.blocks, block{sum: binary.BigEndian.Uint64(sum[:8]), marker: marker(s)})
			f.u.amounts = append(f.u.amounts, amounts(s)...)
			f.u.dates = append(f.u.dates, dates(s)...)
		}
	}
	flushLoose := func(f *frame) {
		s := normalise(f.loose.String())
		f.loose.Reset()
		addBlock(f, s)
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
				return &watch.Error{Code: watch.CodeTimeout, Message: "the source took too long to read"}
			}
			return parseError("the source is not well-formed XML")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if rootDone {
				return parseError("the source has content after its root element")
			}
			if inBlock {
				if isDivision(t.Name) {
					return needsSubtool("a division sits inside a text element")
				}
				blockDepth++
				continue
			}
			if len(stack) == 0 || isDivision(t.Name) {
				label, ok := divisionLabel(t)
				if !ok {
					if len(stack) == 0 {
						return needsSubtool("the source is XML but its root is not an eCFR division")
					}
					return needsSubtool("a division has an unknown TYPE or no N")
				}
				u := &unit{head: emptyHead}
				if len(stack) > 0 {
					parent := stack[len(stack)-1]
					flushLoose(parent)
					if parent.children[label] {
						return needsSubtool("two divisions under one parent share a TYPE and N")
					}
					parent.children[label] = true
					u.key = parent.u.key + keySep + label
				} else {
					u.key = label
				}
				u.seq = seq
				seq++
				u.detail = wantDetail != nil && wantDetail(u.key)
				stack = append(stack, &frame{u: u, h: sha256.New(), children: map[string]bool{}})
				continue
			}
			top := stack[len(stack)-1]
			flushLoose(top)
			inBlock, blockDepth = true, 1
			blockHead = t.Name.Local == "HEAD" && t.Name.Space == "" && !top.headSeen
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			if inBlock {
				blockDepth--
				if blockDepth > 0 {
					continue
				}
				inBlock = false
				s := normalise(text.String())
				text.Reset()
				if blockHead {
					top.headSeen = true
					top.u.head = sha256.Sum256([]byte(s))
					top.u.reserved = strings.Contains(strings.ToLower(s), "[reserved]")
					if top.u.detail {
						top.u.amounts = append(top.u.amounts, amounts(s)...)
						top.u.dates = append(top.u.dates, dates(s)...)
					}
				} else {
					addBlock(top, s)
				}
				continue
			}
			flushLoose(top)
			top.h.Sum(top.u.body[:0])
			emit(top.u)
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootDone = true
			}
		case xml.CharData:
			switch {
			case inBlock:
				if text.Len()+len(t) > maxBlockBytes {
					return needsSubtool("one text element is larger than this watcher handles")
				}
				text.Write(t)
			case len(stack) > 0:
				top := stack[len(stack)-1]
				if top.loose.Len()+len(t) > maxBlockBytes {
					return needsSubtool("one text element is larger than this watcher handles")
				}
				top.loose.Write(t)
			case strings.TrimSpace(string(t)) != "":
				return parseError("the source has text outside its root element")
			}
		}
	}
	if !rootDone {
		return parseError("the source is empty or has no root element")
	}
	return nil
}
