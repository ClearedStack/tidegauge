package federalregister

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

var update = flag.Bool("update", false, "rewrite want.json in every fixture from current behaviour")

func readOptional(t *testing.T, path string) string {
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

func openSide(t *testing.T, dir, prefix string) *os.File {
	t.Helper()
	for _, ext := range []string{".json", ".xml"} {
		f, err := os.Open(filepath.Join(dir, prefix+ext))
		if err == nil {
			return f
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Fatalf("no %s.json or %s.xml in %s", prefix, prefix, dir)
	return nil
}

func runFixture(t *testing.T, dir string) []byte {
	t.Helper()
	oldFile := openSide(t, dir, "old")
	defer oldFile.Close()
	newFile := openSide(t, dir, "new")
	defer newFile.Close()

	report := Compare(
		Snapshot{ETag: readOptional(t, filepath.Join(dir, "old.etag")), Body: oldFile},
		Snapshot{ETag: readOptional(t, filepath.Join(dir, "new.etag")), Body: newFile},
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

func TestFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures found (%v)", err)
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
				t.Fatalf("fixture %q does not state an outcome", name)
			}
			gotCode := ""
			if r.Error != nil {
				gotCode = r.Error.Code
			}
			if r.Status != wantStatus || gotCode != wantCode {
				t.Errorf("status %q code %q, want status %q code %q", r.Status, gotCode, wantStatus, wantCode)
			}
		})
	}
}

func TestSameInputSameOutput(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join("testdata", "*"))
	for _, dir := range dirs {
		if a, b := runFixture(t, dir), runFixture(t, dir); !bytes.Equal(a, b) {
			t.Errorf("%s: non-deterministic output", dir)
		}
	}
}

type deadlineReader struct{}

func (deadlineReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestTimeoutAndSanitization(t *testing.T) {
	good := `{"results":[{"document_number":"2031-90101","type":"Rule","title":"Sample Rule"}]}`
	r := Compare(Snapshot{Body: strings.NewReader(good)}, Snapshot{Body: deadlineReader{}})
	if r.Error == nil || r.Error.Code != watch.CodeTimeout || r.Status != watch.StatusFailed {
		t.Fatalf("expected timeout failure, got %+v", r)
	}
	secret := "node-secret-99.internal.example"
	bad := `{"results":[{"document_number":"` + secret
	r2 := Compare(Snapshot{Body: strings.NewReader(good)}, Snapshot{Body: strings.NewReader(bad)})
	if r2.Error == nil || r2.Error.Code != watch.CodeParse {
		t.Fatalf("expected parse failure, got %+v", r2)
	}
	if strings.Contains(r2.Error.Message, secret) {
		t.Fatalf("error leaked source text: %q", r2.Error.Message)
	}
}

func TestMatchURLAndInspect(t *testing.T) {
	if !MatchURL("https://www.federalregister.gov/api/v1/documents.json") {
		t.Fatal("expected federalregister.gov URL to match")
	}
	if MatchURL("https://www.ecfr.gov/api/versioner/v1/full/2031-01-01/title-99.xml") {
		t.Fatal("ecfr URL should not match federal-register")
	}
	sum, werr := Inspect(strings.NewReader(`{"document_number":"2031-90999","type":"Rule","action":"Final rule; repeal of clause","title":"Made-up Repeal","cfr_references":[{"title":99,"part":952}]}`))
	if werr != nil {
		t.Fatalf("Inspect error: %v", werr)
	}
	if !sum.Repealed || sum.DocCount != 1 || sum.Digest == "" {
		t.Fatalf("unexpected Summary: %+v", sum)
	}
}
