// Package watch holds what every Tidegauge sub-tool shares: the three public
// error codes, the outcome names a check can end with, and small deterministic
// helpers for ETags, whitespace, dollar amounts and dates.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The three public error codes. There are no others.
const (
	CodeTimeout      = "E_TIMEOUT"       // something outside took too long
	CodeParse        = "E_PARSE"         // the data was broken
	CodeNeedsSubtool = "E_NEEDS_SUBTOOL" // the data was valid and unfamiliar
)

// Outcomes of one check.
const (
	StatusUnchanged    = "unchanged"
	StatusChanged      = "changed"
	StatusFailed       = "failed"        // E_TIMEOUT or E_PARSE
	StatusNeedsSubtool = "needs_subtool" // E_NEEDS_SUBTOOL
)

// Error is a public, sanitized failure. Message is always a fixed sentence
// written in this repository: it never carries text from the source, a file
// path, a host name, an address or a credential.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// StatusFor gives the outcome a failed check ends with.
func StatusFor(e *Error) string {
	if e.Code == CodeNeedsSubtool {
		return StatusNeedsSubtool
	}
	return StatusFailed
}

// SameETag reports whether two HTTP ETag header values name the same version.
// Both must be present and well formed. A leading "W/" weak validator is
// ignored, matching conditional GET semantics.
func SameETag(a, b string) bool {
	x, ok := etagOpaque(a)
	if !ok {
		return false
	}
	y, ok := etagOpaque(b)
	return ok && x == y
}

func etagOpaque(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' || strings.ContainsAny(s[1:len(s)-1], "\" \t\r\n") {
		return "", false
	}
	return s, true
}

// Normalise collapses every run of white space to one space and trims both ends.
func Normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// IsTimeout reports whether err represents a read or context deadline.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// ReadBounded reads up to maxBytes from r. A deadline error becomes E_TIMEOUT,
// a body over maxBytes becomes E_NEEDS_SUBTOOL, and any other read failure
// becomes E_PARSE.
func ReadBounded(r io.Reader, maxBytes int) ([]byte, *Error) {
	if r == nil {
		return nil, &Error{Code: CodeParse, Message: "the saved version is missing"}
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		if IsTimeout(err) {
			return nil, &Error{Code: CodeTimeout, Message: "the source took too long to read"}
		}
		return nil, &Error{Code: CodeParse, Message: "the source could not be read"}
	}
	if len(data) > maxBytes {
		return nil, &Error{Code: CodeNeedsSubtool, Message: "the source is larger than this watcher handles"}
	}
	return data, nil
}

var amountRE = regexp.MustCompile(`\$(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?(?: (?:thousand|million|billion)\b)?`)

// Amounts returns dollar amounts in s as written, cloned from the input.
func Amounts(s string) []string {
	found := amountRE.FindAllString(s, -1)
	for i := range found {
		found[i] = strings.Clone(found[i])
	}
	return found
}

var months = map[string]time.Month{
	"January": 1, "February": 2, "March": 3, "April": 4, "May": 5, "June": 6,
	"July": 7, "August": 8, "September": 9, "October": 10, "November": 11, "December": 12,
	"Jan.": 1, "Feb.": 2, "Mar.": 3, "Apr.": 4, "Aug.": 8, "Sept.": 9, "Sep.": 9,
	"Oct.": 10, "Nov.": 11, "Dec.": 12,
}

var (
	textDateRE = regexp.MustCompile(`\b(January|February|March|April|May|June|July|August|September|October|November|December|Jan\.|Feb\.|Mar\.|Apr\.|Aug\.|Sept\.|Sep\.|Oct\.|Nov\.|Dec\.) ([0-9]{1,2}), ([0-9]{4})\b`)
	isoDateRE  = regexp.MustCompile(`\b([0-9]{4})-([0-9]{2})-([0-9]{2})\b`)
)

// Dates extracts valid calendar dates from s in YYYY-MM-DD form, recognizing
// both written US dates ("March 3, 2031", "Jan. 5, 2030") and ISO dates
// ("2031-03-03").
func Dates(s string) []string {
	var out []string
	for _, m := range textDateRE.FindAllStringSubmatch(s, -1) {
		day, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		month := months[m[1]]
		if validYMD(year, month, day) {
			out = append(out, fmt.Sprintf("%04d-%02d-%02d", year, int(month), day))
		}
	}
	for _, m := range isoDateRE.FindAllStringSubmatch(s, -1) {
		year, _ := strconv.Atoi(m[1])
		mon, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if mon >= 1 && mon <= 12 && validYMD(year, time.Month(mon), day) {
			out = append(out, fmt.Sprintf("%04d-%02d-%02d", year, mon, day))
		}
	}
	return out
}

func validYMD(year int, month time.Month, day int) bool {
	d := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return d.Year() == year && d.Month() == month && d.Day() == day
}

// Sorted returns a sorted copy of s, or nil when s is empty.
func Sorted(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

// SortedUnique returns sorted distinct strings from s, or nil when empty.
func SortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// MultisetDiff returns elements in a not in b and elements in b not in a,
// counting multiplicity, each sorted.
func MultisetDiff(a, b []string) (onlyA, onlyB []string) {
	count := make(map[string]int, len(a))
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		if count[s] > 0 {
			count[s]--
		} else {
			onlyB = append(onlyB, s)
		}
	}
	for _, s := range a {
		if count[s] > 0 {
			count[s]--
			onlyA = append(onlyA, s)
		}
	}
	return Sorted(onlyA), Sorted(onlyB)
}
