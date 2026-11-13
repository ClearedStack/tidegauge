package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

var fixtures = filepath.Join("..", "..", "tools", "ecfr", "testdata")

func TestRun(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		etags   []string
		exit    int
	}{
		{"changed", "changed-threshold", nil, 0},
		{"unchanged", "unchanged-identical", nil, 0},
		{"etag", "unchanged-etag-match", []string{"-old-etag", `"made-up-v1"`, "-new-etag", `W/"made-up-v1"`}, 0},
		{"parse", "parse-truncated", nil, 1},
		{"needs_subtool", "needs-subtool-not-ecfr", nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(fixtures, c.fixture)
			args := append([]string{"ecfr", "-old", filepath.Join(dir, "old.xml"), "-new", filepath.Join(dir, "new.xml")}, c.etags...)
			var stdout, stderr bytes.Buffer
			if got := run(args, &stdout, &stderr); got != c.exit {
				t.Errorf("exit %d, want %d (stderr: %s)", got, c.exit, stderr.String())
			}
			want, err := os.ReadFile(filepath.Join(dir, "want.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stdout.Bytes(), want) {
				t.Errorf("output differs from want.json\n%s", stdout.String())
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "host-zz9-missing.xml")
	for _, args := range [][]string{
		nil,
		{"federal-register"},
		{"ecfr"},
		{"ecfr", "-old", "a.xml"},
		{"ecfr", "-nope"},
		{"ecfr", "-old", missing, "-new", missing},
	} {
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != 2 {
			t.Errorf("%v: exit %d, want 2", args, got)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: wrote to stdout", args)
		}
		if bytes.Contains(stderr.Bytes(), []byte("host-zz9")) {
			t.Errorf("%v: message names a path: %s", args, stderr.String())
		}
	}
}
