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

func TestRunAllSubtoolsAndWorkStandin(t *testing.T) {
	subtoolCases := []struct {
		tool    string
		fixture string
		ext     string
		extra   []string
	}{
		{"federal-register", "changed-json-new-final-rule", ".json", nil},
		{"nist-csrc", "changed-atom-revision", ".xml", nil},
		{"linked-tool-watch", "changed-paywall-dom", ".html", []string{"-origin", "https://synth-calc.example"}},
		{"live-catalog", "changed-json-rule-version-drift", ".json", nil},
	}
	for _, tc := range subtoolCases {
		t.Run(tc.tool, func(t *testing.T) {
			dir := filepath.Join("..", "..", "tools", tc.tool, "testdata", tc.fixture)
			args := append([]string{
				tc.tool,
				"-old", filepath.Join(dir, "old"+tc.ext),
				"-new", filepath.Join(dir, "new"+tc.ext),
			}, tc.extra...)
			var stdout, stderr bytes.Buffer
			if got := run(args, &stdout, &stderr); got != 0 {
				t.Fatalf("exit %d, want 0 (stderr: %s)", got, stderr.String())
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

	// Verify standalone worker mode (-standin -once) runs cleanly with local spool.
	var stdout, stderr bytes.Buffer
	code := run([]string{"work", "-standin", "-once", "-spool", t.TempDir()}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("tidegauge work -standin -once exited %d: %s", code, stderr.String())
	}
}

func TestRunUsage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "host-zz9-missing.xml")
	for _, args := range [][]string{
		nil,
		{"unknown-subcommand"},
		{"federal-register"},
		{"ecfr"},
		{"ecfr", "-old", "a.xml"},
		{"ecfr", "-nope"},
		{"ecfr", "-old", missing, "-new", missing},
		{"work", "-poll", "not-a-duration"},
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
