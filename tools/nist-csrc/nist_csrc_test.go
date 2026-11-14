package nistcsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

var update = flag.Bool("update", false, "rewrite testdata/*/want.json from Compare's output")

func findInput(t *testing.T, dir, prefix string) string {
	t.Helper()
	for _, ext := range []string{".xml", ".html"} {
		p := filepath.Join(dir, prefix+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("no %s.{xml,html} in %s", prefix, dir)
	return ""
}

func readETag(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join("testdata", entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			oldBytes, err := os.ReadFile(findInput(t, dir, "old"))
			if err != nil {
				t.Fatal(err)
			}
			newBytes, err := os.ReadFile(findInput(t, dir, "new"))
			if err != nil {
				t.Fatal(err)
			}
			gotReport := Compare(
				Snapshot{ETag: readETag(dir, "old.etag"), Body: bytes.NewReader(oldBytes)},
				Snapshot{ETag: readETag(dir, "new.etag"), Body: bytes.NewReader(newBytes)},
			)
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(gotReport); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()

			// Verify synthetic prose never leaks into report JSON.
			for _, forbidden := range []string{"SYNTHETIC_SENTINEL", "Protecting Synthetic Controlled", "Errata clarifying synthetic"} {
				if bytes.Contains(got, []byte(forbidden)) {
					t.Fatalf("report leaked source prose %q:\n%s", forbidden, got)
				}
			}

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
				t.Errorf("Compare output differs from want.json\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("no fixtures under testdata/")
	}
}

func TestSameInputSameOutput(t *testing.T) {
	dir := filepath.Join("testdata", "changed-atom-revision")
	oldBytes, err := os.ReadFile(filepath.Join(dir, "old.xml"))
	if err != nil {
		t.Fatal(err)
	}
	newBytes, err := os.ReadFile(filepath.Join(dir, "new.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 10; i++ {
		r := Compare(
			Snapshot{Body: bytes.NewReader(oldBytes)},
			Snapshot{Body: bytes.NewReader(newBytes)},
		)
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Fatalf("run %d differed from run 0", i)
		}
	}
}

type timeoutReader struct{}

func (timeoutReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

type leakReader struct{}

func (leakReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestTimeoutAndOversize(t *testing.T) {
	valid := `<rss version="2.0"><channel><title>Synthetic CSRC</title><item><guid>sp-999-171</guid><title>SP 999-171 Rev. 2</title><pubDate>2031-01-10</pubDate></item></channel></rss>`
	got := Compare(
		Snapshot{Body: strings.NewReader(valid)},
		Snapshot{Body: timeoutReader{}},
	)
	if got.Error == nil || got.Error.Code != watch.CodeTimeout {
		t.Fatalf("got %+v, want E_TIMEOUT", got)
	}

	gotLeak := Compare(
		Snapshot{Body: strings.NewReader(valid)},
		Snapshot{Body: leakReader{}},
	)
	if gotLeak.Error == nil || gotLeak.Error.Code != watch.CodeParse {
		t.Fatalf("got %+v, want E_PARSE", gotLeak)
	}
}

func TestMatchURLAndInspect(t *testing.T) {
	if !MatchURL("https://csrc.nist.gov/news/2031/sp-999-171") {
		t.Fatal("expected csrc.nist.gov to match")
	}
	if !MatchURL("https://pages.nist.gov/sp-999-171/feed.xml") {
		t.Fatal("expected pages.nist.gov to match")
	}
	if MatchURL("https://www.ecfr.gov/api/versioner/v1/full/2031-01-01/title-48.xml") {
		t.Fatal("unexpected match for ecfr.gov")
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "changed-dom-withdrawn", "new.html"))
	if err != nil {
		t.Fatal(err)
	}
	sum, werr := Inspect(bytes.NewReader(raw))
	if werr != nil {
		t.Fatalf("Inspect failed: %+v", werr)
	}
	if sum.Format != "dom" || sum.PubCount != 1 || !sum.Repealed || sum.Digest == "" {
		t.Fatalf("unexpected Summary: %+v", sum)
	}
}
