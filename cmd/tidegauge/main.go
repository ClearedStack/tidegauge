// Command tidegauge runs either the continuous Muster monitoring worker
// ("tidegauge work") or one deterministic saved-file comparison for any of its
// five sub-tools ("ecfr", "federal-register", "nist-csrc", "linked-tool-watch",
// "live-catalog").
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/tidegauge/internal/muster"
	"github.com/ClearedStack/tidegauge/internal/spool"
	"github.com/ClearedStack/tidegauge/internal/watch"
	"github.com/ClearedStack/tidegauge/internal/worker"
	"github.com/ClearedStack/tidegauge/tools/ecfr"
	federalregister "github.com/ClearedStack/tidegauge/tools/federal-register"
	linkedtoolwatch "github.com/ClearedStack/tidegauge/tools/linked-tool-watch"
	livecatalog "github.com/ClearedStack/tidegauge/tools/live-catalog"
	nistcsrc "github.com/ClearedStack/tidegauge/tools/nist-csrc"
)

const defaultSoftMemLimit = 24 << 20 // 24 MiB soft heap limit inside 30 MiB budget

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultSoftMemLimit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = "usage: tidegauge work [-muster URL] [-spool DIR] [-token-file PATH] [-standin] [-once]\n" +
	"       tidegauge <ecfr|federal-register|nist-csrc|linked-tool-watch|live-catalog> -old FILE -new FILE [-old-etag VALUE] [-new-etag VALUE]"

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "work":
		return runWork(ctx, args[1:], stderr)
	case ecfr.Name, federalregister.Name, nistcsrc.Name, linkedtoolwatch.Name, livecatalog.Name:
		return runCompare(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
}

func runWork(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }

	musterURL := fs.String("muster", envOr("MUSTER_URL", "http://muster.clearedstack.svc.cluster.local:8080"), "Muster base URL")
	spoolDir := fs.String("spool", envOr("TIDEGAUGE_SPOOL_DIR", spool.DefaultDir), "local spool directory")
	tokenFile := fs.String("token-file", envOr("TIDEGAUGE_TOKEN_FILE", muster.DefaultTokenFile), "ServiceAccount token path")
	pollStr := fs.String("poll", envOr("TIDEGAUGE_POLL_INTERVAL", "5s"), "idle poll interval")
	standinMode := fs.Bool("standin", false, "run against an in-process Muster contract v1 stand-in")
	once := fs.Bool("once", false, "run a single lease check cycle and exit")

	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, usage)
		}
		return 2
	}
	pollInterval, err := time.ParseDuration(*pollStr)
	if err != nil || pollInterval <= 0 {
		fmt.Fprintln(stderr, "tidegauge: invalid poll interval")
		return 2
	}

	sp, err := spool.New(*spoolDir)
	if err != nil {
		fmt.Fprintln(stderr, "tidegauge: cannot initialize spool directory")
		return 1
	}

	var client *muster.Client
	if *standinMode {
		standin := muster.NewStandin(nil)
		srv := httptest.NewServer(standin)
		defer srv.Close()
		client, err = muster.NewClient(srv.URL, muster.StaticTokenSource(standin.TokenFor("tidegauge")), srv.Client())
	} else {
		client, err = muster.NewClient(*musterURL, muster.FileTokenSource(*tokenFile), nil)
	}
	if err != nil {
		fmt.Fprintln(stderr, "tidegauge: invalid Muster client configuration")
		return 2
	}

	w := worker.New(worker.Config{
		Muster:       client,
		Spool:        sp,
		PollInterval: pollInterval,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(stderr, format+"\n", a...)
		},
	})

	if *once {
		if _, err := w.StepOnce(ctx); err != nil {
			fmt.Fprintln(stderr, "tidegauge: worker cycle failed")
			return 1
		}
		return 0
	}
	if err := w.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "tidegauge: worker stopped with error")
		return 1
	}
	return 0
}

func runCompare(subtool string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(subtool, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }

	oldPath := fs.String("old", "", "the earlier saved version")
	newPath := fs.String("new", "", "the later saved version")
	oldETag := fs.String("old-etag", "", "the ETag header saved with the earlier version")
	newETag := fs.String("new-etag", "", "the ETag header saved with the later version")
	oldStatus := fs.Int("old-status", 200, "HTTP status code for the earlier version")
	newStatus := fs.Int("new-status", 200, "HTTP status code for the later version")
	origin := fs.String("origin", "", "origin URL for script classification")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *oldPath == "" || *newPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	oldFile, err := os.Open(*oldPath)
	if err != nil {
		fmt.Fprintln(stderr, "tidegauge: cannot open the file given to -old")
		return 2
	}
	defer oldFile.Close()

	newFile, err := os.Open(*newPath)
	if err != nil {
		fmt.Fprintln(stderr, "tidegauge: cannot open the file given to -new")
		return 2
	}
	defer newFile.Close()

	var report any
	var werr *watch.Error

	switch subtool {
	case ecfr.Name:
		r := ecfr.Compare(
			ecfr.Snapshot{ETag: *oldETag, Body: oldFile},
			ecfr.Snapshot{ETag: *newETag, Body: newFile},
		)
		report, werr = r, r.Error

	case federalregister.Name:
		r := federalregister.Compare(
			federalregister.Snapshot{ETag: *oldETag, Body: oldFile},
			federalregister.Snapshot{ETag: *newETag, Body: newFile},
		)
		report, werr = r, r.Error

	case nistcsrc.Name:
		r := nistcsrc.Compare(
			nistcsrc.Snapshot{ETag: *oldETag, Body: oldFile},
			nistcsrc.Snapshot{ETag: *newETag, Body: newFile},
		)
		report, werr = r, r.Error

	case linkedtoolwatch.Name:
		r := linkedtoolwatch.Compare(
			linkedtoolwatch.Snapshot{StatusCode: *oldStatus, ETag: *oldETag, Origin: *origin, Body: oldFile},
			linkedtoolwatch.Snapshot{StatusCode: *newStatus, ETag: *newETag, Origin: *origin, Body: newFile},
		)
		report, werr = r, r.Error

	case livecatalog.Name:
		r := livecatalog.Compare(
			livecatalog.Snapshot{StatusCode: *oldStatus, ETag: *oldETag, Body: oldFile},
			livecatalog.Snapshot{StatusCode: *newStatus, ETag: *newETag, Body: newFile},
		)
		report, werr = r, r.Error
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(report); err != nil {
		return 1
	}
	if werr != nil {
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
