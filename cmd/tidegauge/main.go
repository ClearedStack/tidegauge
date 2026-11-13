// Command tidegauge runs one Tidegauge check from the command line.
//
// Today it has one sub-tool, ecfr, which compares two saved versions of an
// eCFR source:
//
//	tidegauge ecfr -old old.xml -new new.xml
//
// The report is printed as JSON. Exit status: 0 when the check ran
// (changed or unchanged), 1 when it ended in an error code, 2 for a usage
// mistake.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/clearedstack/tidegauge/tools/ecfr"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = "usage: tidegauge ecfr -old FILE -new FILE [-old-etag VALUE] [-new-etag VALUE]"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != ecfr.Name {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet(ecfr.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	oldPath := fs.String("old", "", "the earlier saved version (XML file)")
	newPath := fs.String("new", "", "the later saved version (XML file)")
	oldETag := fs.String("old-etag", "", "the ETag header saved with the earlier version")
	newETag := fs.String("new-etag", "", "the ETag header saved with the later version")
	if err := fs.Parse(args[1:]); err != nil {
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

	report := ecfr.Compare(
		ecfr.Snapshot{ETag: *oldETag, Body: oldFile},
		ecfr.Snapshot{ETag: *newETag, Body: newFile},
	)
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(report); err != nil {
		return 1
	}
	if report.Error != nil {
		return 1
	}
	return 0
}
