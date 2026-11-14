package ecfr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// normalise collapses every run of white space to one space and trims the
// ends, so a change in line breaks or indentation is not a change.
func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// markerRE finds a leading paragraph marker such as "(a)" or "(b)(2)".
var markerRE = regexp.MustCompile(`^(?:\([0-9A-Za-z]{1,6}\))+`)

// marker returns a copy, so the paragraph it came from is not kept alive.
func marker(s string) string { return strings.Clone(markerRE.FindString(s)) }

// amountRE finds a dollar amount: "$12,500", "$0.25", "$2.5 million".
var amountRE = regexp.MustCompile(`\$(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?(?: (?:thousand|million|billion)\b)?`)

// amounts returns the dollar amounts in s exactly as written. They are
// copies, so the paragraph they came from is not kept alive.
func amounts(s string) []string {
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

// dateRE finds a date written as "March 3, 2031" or "Jan. 5, 2030".
var dateRE = regexp.MustCompile(`\b(January|February|March|April|May|June|July|August|September|October|November|December|Jan\.|Feb\.|Mar\.|Apr\.|Aug\.|Sept\.|Sep\.|Oct\.|Nov\.|Dec\.) ([0-9]{1,2}), ([0-9]{4})\b`)

// dates returns the calendar dates in s as YYYY-MM-DD. Text shaped like a
// date that is not a real day (February 30) is left out; it still counts
// as text.
func dates(s string) []string {
	var out []string
	for _, m := range dateRE.FindAllStringSubmatch(s, -1) {
		day, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		month := months[m[1]]
		d := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		if d.Year() != year || d.Month() != month || d.Day() != day {
			continue
		}
		out = append(out, fmt.Sprintf("%04d-%02d-%02d", year, int(month), day))
	}
	return out
}
