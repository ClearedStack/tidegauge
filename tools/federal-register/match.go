package federalregister

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MatchURL reports whether raw identifies a Federal Register source.
func MatchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "federalregister.gov" || strings.HasSuffix(host, ".federalregister.gov") {
		return true
	}
	return strings.HasPrefix(u.Path, "/federal-register/")
}

// Known Federal Register document types. Anything else fails closed as
// E_NEEDS_SUBTOOL rather than guessing.
var docTypes = map[string]string{
	"Rule":                  "rule",
	"RULE":                  "rule",
	"Proposed Rule":         "proposed_rule",
	"PRORULE":               "proposed_rule",
	"Notice":                "notice",
	"NOTICE":                "notice",
	"Presidential Document": "presidential_document",
}

func mapDocType(raw string) (string, bool) {
	k, ok := docTypes[strings.TrimSpace(raw)]
	return k, ok
}

// classifyAction maps a Federal Register ACTION line to a deterministic
// category without retaining raw source text.
func classifyAction(action string, docKind string) string {
	lower := strings.ToLower(action)
	switch {
	case strings.Contains(lower, "repeal") || strings.Contains(lower, "rescind") || strings.Contains(lower, "removal of"):
		return "repeal"
	case strings.Contains(lower, "withdraw"):
		return "withdrawal"
	case strings.Contains(lower, "interim"):
		return "interim_rule"
	case strings.Contains(lower, "final"):
		return "final_rule"
	case strings.Contains(lower, "proposed"):
		return "proposed_rule"
	case docKind == "notice":
		return "notice"
	case lower == "":
		return docKind
	default:
		return "other"
	}
}

var (
	cfrLineRE = regexp.MustCompile(`\b([0-9]{1,3})\s+CFR\s+(?:[Pp]arts?\s+)?([0-9]+(?:\.[0-9]+)?(?:\s*(?:,|and|to|-)\s*[0-9]+(?:\.[0-9]+)?)*)`)
	partNumRE = regexp.MustCompile(`\b[0-9]+(?:\.[0-9]+)?\b`)
	rinRE     = regexp.MustCompile(`\b[0-9]{4}-[A-Z0-9]{4}\b`)
	frDocRE   = regexp.MustCompile(`\b(?:[0-9]{4}-[0-9]{4,6}|[A-Z0-9]{2}-[0-9]{4,6})\b`)
)

// extractCFR parses canonical "TITLE CFR PART" tokens from a `<CFR>` element.
func extractCFR(s string) []string {
	var out []string
	for _, m := range cfrLineRE.FindAllStringSubmatch(s, -1) {
		title := m[1]
		parts := partNumRE.FindAllString(m[2], -1)
		if len(parts) == 0 {
			out = append(out, fmt.Sprintf("%s CFR", title))
			continue
		}
		for _, p := range parts {
			out = append(out, fmt.Sprintf("%s CFR %s", title, p))
		}
	}
	return out
}

func extractRINs(s string) []string {
	found := rinRE.FindAllString(strings.ToUpper(s), -1)
	for i := range found {
		found[i] = strings.Clone(found[i])
	}
	return found
}

func extractDocNum(s string) string {
	if m := frDocRE.FindString(s); m != "" {
		return strings.Clone(m)
	}
	return ""
}
