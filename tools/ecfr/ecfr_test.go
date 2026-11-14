package ecfr

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

var update = flag.Bool("update", false, "rewrite want.json in every fixture from the current behaviour")

// readETag returns a saved ETag header value, or "" when none was saved.
func readETag(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func runFixture(t *testing.T, dir string) []byte {
	t.Helper()
	oldBody, err := os.Open(filepath.Join(dir, "old.xml"))
	if err != nil {
		t.Fatal(err)
	}
	defer oldBody.Close()
	newBody, err := os.Open(filepath.Join(dir, "new.xml"))
	if err != nil {
		t.Fatal(err)
	}
	defer newBody.Close()
	report := Compare(
		Snapshot{ETag: readETag(t, filepath.Join(dir, "old.etag")), Body: oldBody},
		Snapshot{ETag: readETag(t, filepath.Join(dir, "new.etag")), Body: newBody},
	)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(report); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestFixtures runs every saved example under testdata: two saved versions
// of a source in, the exact report expected out.
func TestFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got := runFixture(t, dir)
			wantPath := filepath.Join(dir, "want.json")
			if *update {
				if err := os.WriteFile(wantPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("report differs from %s\n--- got ---\n%s--- want ---\n%s", wantPath, got, want)
			}
			// The fixture's name states its outcome; hold the report to it.
			var r Report
			if err := json.Unmarshal(got, &r); err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(dir)
			var wantStatus, wantCode string
			switch {
			case strings.HasPrefix(name, "unchanged-"):
				wantStatus = watch.StatusUnchanged
			case strings.HasPrefix(name, "changed-"):
				wantStatus = watch.StatusChanged
			case strings.HasPrefix(name, "parse-"):
				wantStatus, wantCode = watch.StatusFailed, watch.CodeParse
			case strings.HasPrefix(name, "needs-subtool-"):
				wantStatus, wantCode = watch.StatusNeedsSubtool, watch.CodeNeedsSubtool
			default:
				t.Fatalf("fixture name %q does not state an outcome", name)
			}
			gotCode := ""
			if r.Error != nil {
				gotCode = r.Error.Code
			}
			if r.Status != wantStatus || gotCode != wantCode {
				t.Errorf("status %q code %q, fixture name says status %q code %q", r.Status, gotCode, wantStatus, wantCode)
			}
		})
	}
}

// TestSameInputSameOutput runs every fixture twice and expects identical bytes.
func TestSameInputSameOutput(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join("testdata", "*"))
	for _, dir := range dirs {
		if a, b := runFixture(t, dir), runFixture(t, dir); !bytes.Equal(a, b) {
			t.Errorf("%s: two runs gave different reports", dir)
		}
	}
}

// TestErrorsCarryNoSourceText checks that a failure message is one of the
// fixed sentences and holds nothing from the document.
func TestErrorsCarryNoSourceText(t *testing.T) {
	const secret = "host-zz9.internal.example"
	inputs := []string{
		"<DIV5 N=\"1\" TYPE=\"PART\"><P>" + secret,
		"<" + secret + "/>",
		"<DIV5 N=\"1\" TYPE=\"" + secret + "\"/>",
		"<DIV5 N=\"1\" TYPE=\"PART\"><DIV8 N=\"" + secret + "\" TYPE=\"X\"/></DIV5>",
	}
	for _, in := range inputs {
		err := parse(strings.NewReader(in), nil, func(*unit) {})
		if err == nil {
			t.Fatalf("expected a failure for %q", in)
		}
		if strings.Contains(err.Message, secret) || strings.Contains(err.Message, "line ") {
			t.Errorf("message leaks source detail: %q", err.Message)
		}
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error)       { return 0, f.err }
func (f failingReader) Seek(int64, int) (int64, error) { return 0, nil }

// TestSlowSourceIsTimeout: a body whose read hits a deadline is E_TIMEOUT;
// any other broken read is E_PARSE.
func TestSlowSourceIsTimeout(t *testing.T) {
	good := `<DIV5 N="901" TYPE="PART"><HEAD>Made-up</HEAD></DIV5>`
	cases := []struct {
		err          error
		code, status string
	}{
		{context.DeadlineExceeded, watch.CodeTimeout, watch.StatusFailed},
		{fmt.Errorf("wrapped: %w", os.ErrDeadlineExceeded), watch.CodeTimeout, watch.StatusFailed},
		{io.ErrUnexpectedEOF, watch.CodeParse, watch.StatusFailed},
	}
	for _, c := range cases {
		r := Compare(Snapshot{Body: strings.NewReader(good)}, Snapshot{Body: failingReader{c.err}})
		if r.Error == nil || r.Error.Code != c.code || r.Status != c.status {
			t.Errorf("%v: got %+v", c.err, r)
		}
	}
}

// TestOversizeElementNeedsSubtool lowers the per-element cap and checks
// that an element past it pauses the check instead of being guessed at.
func TestOversizeElementNeedsSubtool(t *testing.T) {
	saved := maxBlockBytes
	maxBlockBytes = 64
	defer func() { maxBlockBytes = saved }()
	for _, in := range []string{
		`<DIV5 N="901" TYPE="PART"><P>` + strings.Repeat("made-up ", 20) + `</P></DIV5>`,
		`<DIV5 N="901" TYPE="PART">` + strings.Repeat("made-up ", 20) + `</DIV5>`,
	} {
		err := parse(strings.NewReader(in), nil, func(*unit) {})
		if err == nil || err.Code != watch.CodeNeedsSubtool {
			t.Errorf("got %v, want %s", err, watch.CodeNeedsSubtool)
		}
	}
	if err := parse(strings.NewReader(`<DIV5 N="901" TYPE="PART"><P>short</P></DIV5>`), nil, func(*unit) {}); err != nil {
		t.Errorf("a small element failed: %v", err)
	}
}

func blocksOf(s string) []block {
	var out []block
	for i, f := range strings.Fields(s) {
		out = append(out, block{sum: uint64(f[0]), marker: fmt.Sprintf("(%d)", i+1)})
	}
	return out
}

func TestDiffBlocks(t *testing.T) {
	rm := func(p int) ParagraphChange {
		return ParagraphChange{Op: KindRemoved, Position: p, Marker: fmt.Sprintf("(%d)", p)}
	}
	add := func(p int) ParagraphChange {
		return ParagraphChange{Op: KindAdded, Position: p, Marker: fmt.Sprintf("(%d)", p)}
	}
	cases := []struct {
		old, new string
		want     []ParagraphChange
	}{
		{"a b c", "a b c", nil},
		{"a b c", "a x c", []ParagraphChange{rm(2), add(2)}},
		{"a b c", "a c", []ParagraphChange{rm(2)}},
		{"a c", "a b c", []ParagraphChange{add(2)}},
		{"", "a b", []ParagraphChange{add(1), add(2)}},
		{"a b", "", []ParagraphChange{rm(1), rm(2)}},
		{"a b c d e", "a c x e f", []ParagraphChange{rm(2), rm(4), add(3), add(5)}},
		{"a b c d", "b c d a", []ParagraphChange{rm(1), add(4)}},
	}
	for _, c := range cases {
		got := diffBlocks(blocksOf(c.old), blocksOf(c.new))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q -> %q: got %+v, want %+v", c.old, c.new, got, c.want)
		}
	}
}

// TestDiffBlocksPastTheTableLimit: when the lining-up table would be too
// large, everything between the unchanged ends is reported.
func TestDiffBlocksPastTheTableLimit(t *testing.T) {
	saved := maxLCSCells
	maxLCSCells = 4
	defer func() { maxLCSCells = saved }()
	got := diffBlocks(blocksOf("a b c d e"), blocksOf("a c x e"))
	want := []ParagraphChange{
		{Op: KindRemoved, Position: 2, Marker: "(2)"},
		{Op: KindRemoved, Position: 3, Marker: "(3)"},
		{Op: KindRemoved, Position: 4, Marker: "(4)"},
		{Op: KindAdded, Position: 2, Marker: "(2)"},
		{Op: KindAdded, Position: 3, Marker: "(3)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestExtract(t *testing.T) {
	text := "(a)(1) Pay $1,250,000, or $40 a day, or $7.5 million, or $0.25; not $ 5. " +
		"From Jan. 5, 2030 to September 30, 2031, not February 30, 2031 or Sept 1, 2031."
	if got, want := marker(text), "(a)(1)"; got != want {
		t.Errorf("marker: got %q, want %q", got, want)
	}
	if got, want := amounts(text), []string{"$1,250,000", "$40", "$7.5 million", "$0.25"}; !reflect.DeepEqual(got, want) {
		t.Errorf("amounts: got %q, want %q", got, want)
	}
	if got, want := dates(text), []string{"2030-01-05", "2031-09-30"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dates: got %q, want %q", got, want)
	}
}

func TestSameETag(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`"v1"`, `"v1"`, true},
		{`W/"v1"`, `"v1"`, true},
		{`"v1"`, `"v2"`, false},
		{``, ``, false},
		{`"v1"`, ``, false},
		{`v1`, `v1`, false},
		{`""`, `""`, false},
		{`"a b"`, `"a b"`, false},
	}
	for _, c := range cases {
		if got := sameETag(c.a, c.b); got != c.want {
			t.Errorf("sameETag(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
