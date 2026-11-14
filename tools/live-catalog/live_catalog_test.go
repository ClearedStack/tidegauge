package livecatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

var update = flag.Bool("update", false, "rewrite testdata/*/want.json from Compare's output")

func findInput(t *testing.T, dir, prefix string) string {
	t.Helper()
	for _, ext := range []string{".json", ".html"} {
		p := filepath.Join(dir, prefix+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("no %s.{json,html} in %s", prefix, dir)
	return ""
}

func readOpt(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readStatus(dir, name string) int {
	s := readOpt(dir, name)
	if s == "" {
		return 200
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
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
				Snapshot{
					StatusCode: readStatus(dir, "old.status"),
					ETag:       readOpt(dir, "old.etag"),
					Body:       bytes.NewReader(oldBytes),
				},
				Snapshot{
					StatusCode: readStatus(dir, "new.status"),
					ETag:       readOpt(dir, "new.etag"),
					Body:       bytes.NewReader(newBytes),
				},
			)
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(gotReport); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()

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

type timeoutReader struct{}

func (timeoutReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestTimeoutAndMatchURL(t *testing.T) {
	if !MatchURL("https://clearedstack.com/tools/synth-threshold-check/") {
		t.Fatal("expected clearedstack.com URL to match")
	}
	if MatchURL("https://csrc.nist.gov/publications/sp") {
		t.Fatal("unexpected match for csrc.nist.gov")
	}

	valid := `{"slug":"synth-threshold-check","status":"live","rule_version":"99-CFR-901@2031-01-15"}`
	got := Compare(
		Snapshot{Body: strings.NewReader(valid)},
		Snapshot{Body: timeoutReader{}},
	)
	if got.Error == nil || got.Error.Code != watch.CodeTimeout {
		t.Fatalf("got %+v, want E_TIMEOUT", got)
	}
}
