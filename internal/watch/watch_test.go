package watch

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestWatchHelpers(t *testing.T) {
	if !SameETag(`"v1"`, `W/"v1"`) || SameETag(`"v1"`, `"v2"`) || SameETag("", "") {
		t.Fatal("SameETag mismatch")
	}
	if got := Normalise("  a\t\n b  "); got != "a b" {
		t.Fatalf("Normalise: got %q", got)
	}
	if got := Amounts("limit $10,000 and $2.5 million"); !reflect.DeepEqual(got, []string{"$10,000", "$2.5 million"}) {
		t.Fatalf("Amounts: got %v", got)
	}
	if got := Dates("Effective March 3, 2031 and 2032-02-29, not 2031-02-29"); !reflect.DeepEqual(got, []string{"2031-03-03", "2032-02-29"}) {
		t.Fatalf("Dates: got %v", got)
	}
	rm, add := MultisetDiff([]string{"b", "a", "a"}, []string{"a", "c"})
	if !reflect.DeepEqual(rm, []string{"a", "b"}) || !reflect.DeepEqual(add, []string{"c"}) {
		t.Fatalf("MultisetDiff: rm=%v add=%v", rm, add)
	}
	if _, werr := ReadBounded(errReader{context.DeadlineExceeded}, 64); werr == nil || werr.Code != CodeTimeout {
		t.Fatalf("expected timeout, got %v", werr)
	}
	if _, werr := ReadBounded(errReader{io.ErrUnexpectedEOF}, 64); werr == nil || werr.Code != CodeParse {
		t.Fatalf("expected parse error, got %v", werr)
	}
	if _, werr := ReadBounded(strings.NewReader("0123456789"), 5); werr == nil || werr.Code != CodeNeedsSubtool {
		t.Fatalf("expected needs_subtool on oversize body, got %v", werr)
	}
}
