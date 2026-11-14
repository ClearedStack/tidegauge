package fetch

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

func TestSSRFRestrictionsAndSanitization(t *testing.T) {
	f := New(Options{ScratchDir: t.TempDir()})
	disallowed := []string{
		"http://www.ecfr.gov/api/versioner/v1/full/2031-01-01/title-48.xml",
		"https://127.0.0.1/feed.xml",
		"https://10.42.0.15:8080/v1/lease",
		"https://169.254.169.254/latest/meta-data/",
		"https://100.64.1.2/internal",
		"https://logbook.clearedstack.svc.cluster.local:5432/",
		"https://user:secret@www.ecfr.gov/feed.xml",
		"file:///etc/passwd",
	}
	for _, raw := range disallowed {
		resp, werr := f.Get(context.Background(), raw, "")
		if resp != nil {
			_ = resp.Close()
			t.Fatalf("expected %q to be rejected", raw)
		}
		if werr == nil || werr.Code != watch.CodeParse {
			t.Fatalf("%q: got %+v, want E_PARSE", raw, werr)
		}
		for _, leak := range []string{"127.0.0.1", "10.42", "169.254", "logbook", "secret", "passwd", "ecfr.gov"} {
			if strings.Contains(werr.Message, leak) {
				t.Fatalf("error leaked %q: %s", leak, werr.Message)
			}
		}
	}
}

func TestConditionalETagAndScratchSpill(t *testing.T) {
	bigPayload := bytes.Repeat([]byte("<DIV8 N=\"901.1\"><P>Synthetic text.</P></DIV8>\n"), 2500) // ~115 KiB > 64 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"etag-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"etag-v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bigPayload)
	}))
	defer srv.Close()

	scratch := t.TempDir()
	f := New(Options{
		ScratchDir:          scratch,
		HTTPClient:          srv.Client(),
		AllowPrivateForTest: true,
	})

	// 1. Full download >64 KiB spills to scratch file and supports multiple Seeks.
	resp, werr := f.Get(context.Background(), srv.URL+"/part-901.xml", "")
	if werr != nil {
		t.Fatalf("Get failed: %+v", werr)
	}
	entries, _ := os.ReadDir(scratch)
	if len(entries) != 1 {
		t.Fatalf("expected 1 scratch file while open, got %d", len(entries))
	}
	firstRead, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(firstRead, bigPayload) {
		t.Fatalf("first read mismatch (len=%d err=%v)", len(firstRead), err)
	}
	if _, err := resp.Body.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	secondRead, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(secondRead, bigPayload) {
		t.Fatalf("second read mismatch")
	}
	if err := resp.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	entriesAfter, _ := os.ReadDir(scratch)
	if len(entriesAfter) != 0 {
		t.Fatalf("expected scratch file removed after Close, got %d", len(entriesAfter))
	}

	// 2. Conditional GET with matching ETag returns 304 NotModified.
	resp304, werr := f.Get(context.Background(), srv.URL+"/part-901.xml", `"etag-v1"`)
	if werr != nil {
		t.Fatalf("conditional Get failed: %+v", werr)
	}
	defer resp304.Close()
	if !resp304.NotModified || resp304.StatusCode != http.StatusNotModified || resp304.ETag != `"etag-v1"` {
		t.Fatalf("unexpected 304 response: %+v", resp304)
	}
}
