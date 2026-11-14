package linkedtoolwatch

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
			oldBytes, err := os.ReadFile(filepath.Join(dir, "old.html"))
			if err != nil {
				t.Fatal(err)
			}
			newBytes, err := os.ReadFile(filepath.Join(dir, "new.html"))
			if err != nil {
				t.Fatal(err)
			}
			origin := readOpt(dir, "origin.txt")
			gotReport := Compare(
				Snapshot{
					StatusCode: readStatus(dir, "old.status"),
					ETag:       readOpt(dir, "old.etag"),
					Origin:     origin,
					Body:       bytes.NewReader(oldBytes),
				},
				Snapshot{
					StatusCode: readStatus(dir, "new.status"),
					ETag:       readOpt(dir, "new.etag"),
					Origin:     origin,
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

			// Clean-room invariant: no external hostnames, URLs, or prose in report.
			for _, forbidden := range []string{
				"synth-calc.example",
				"synth-analytics.example",
				"SYNTHETIC_EXTERNAL_PROSE",
				"googletagmanager",
			} {
				if bytes.Contains(got, []byte(forbidden)) {
					t.Fatalf("report leaked external content %q:\n%s", forbidden, got)
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

type deadlineReader struct{}

func (deadlineReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestReaderTimeoutAndSummary(t *testing.T) {
	freeHTML := `<!DOCTYPE html><html><body><main><form><input name="rate"/></form></main></body></html>`
	got := Compare(
		Snapshot{StatusCode: 200, Body: strings.NewReader(freeHTML)},
		Snapshot{StatusCode: 200, Body: deadlineReader{}},
	)
	if got.Error == nil || got.Error.Code != watch.CodeTimeout {
		t.Fatalf("got %+v, want E_TIMEOUT", got)
	}

	sum, werr := Inspect(Snapshot{
		StatusCode: 200,
		Origin:     "https://synth-calc.example",
		Body:       strings.NewReader(freeHTML),
	})
	if werr != nil {
		t.Fatalf("Inspect failed: %+v", werr)
	}
	if !sum.Available || sum.Paywalled || sum.Digest == "" {
		t.Fatalf("unexpected Summary: %+v", sum)
	}
	raw, _ := json.Marshal(sum)
	if bytes.Contains(raw, []byte("synth-calc.example")) {
		t.Fatalf("Summary leaked hostname: %s", raw)
	}
}
