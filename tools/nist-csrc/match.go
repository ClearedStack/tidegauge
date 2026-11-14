package nistcsrc

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MatchURL reports whether raw identifies a NIST CSRC feed or publication page.
func MatchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "csrc.nist.gov" || strings.HasSuffix(host, ".csrc.nist.gov") || host == "pages.nist.gov" {
		return true
	}
	return strings.HasPrefix(u.Path, "/nist-csrc/")
}

var (
	// Matches NIST publication identifiers such as "NIST SP 999-171", "SP 999-171r3",
	// "SP 999-53A", "FIPS 999-4", "NIST IR 9991", "NISTIR 9991", or "CSWP 99".
	pubIDRE = regexp.MustCompile(`(?i)\b(?:NIST\s+)?(SP|FIPS|NISTIR|IR|CSWP)\s*(?:PUB\s+)?([0-9]{1,5}(?:-[0-9]{1,4}[A-Z]?)?)(?:r([0-9]+))?\b`)
	revRE   = regexp.MustCompile(`(?i)\b(?:rev(?:ision)?\.?\s*([0-9]+)|r([0-9]+))\b`)
	updRE   = regexp.MustCompile(`(?i)\b(?:upd(?:ate)?\.?\s*([0-9]+))\b`)
	verRE   = regexp.MustCompile(`(?i)\bv([0-9]+(?:\.[0-9]+)+)\b`)
)

// pubMeta holds normalized structured metadata extracted from a title/header.
type pubMeta struct {
	pubID    string
	series   string
	revision string
}

func extractPubMeta(texts ...string) (pubMeta, bool) {
	joined := strings.Join(texts, " ")
	m := pubIDRE.FindStringSubmatch(joined)
	if m == nil {
		return pubMeta{}, false
	}
	series := strings.ToUpper(m[1])
	if series == "NISTIR" {
		series = "IR"
	}
	num := strings.ToUpper(m[2])
	inlineRev := m[3]

	rev := ""
	if inlineRev != "" {
		rev = "r" + inlineRev
	} else if rm := revRE.FindStringSubmatch(joined); rm != nil {
		if rm[1] != "" {
			rev = "r" + rm[1]
		} else if rm[2] != "" {
			rev = "r" + rm[2]
		}
	} else if vm := verRE.FindStringSubmatch(joined); vm != nil {
		rev = "v" + vm[1]
	}
	if um := updRE.FindStringSubmatch(joined); um != nil {
		if rev != "" {
			rev = rev + "-upd" + um[1]
		} else {
			rev = "upd" + um[1]
		}
	}

	return pubMeta{
		pubID:    fmt.Sprintf("%s %s", series, num),
		series:   series,
		revision: rev,
	}, true
}

func extractAllPubIDs(s string) []string {
	matches := pubIDRE.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		series := strings.ToUpper(m[1])
		if series == "NISTIR" {
			series = "IR"
		}
		num := strings.ToUpper(m[2])
		id := fmt.Sprintf("%s %s", series, num)
		if m[3] != "" {
			id += "r" + m[3]
		}
		out = append(out, id)
	}
	return out
}

var statusAliases = map[string]string{
	"final":                "final",
	"active":               "final",
	"published":            "final",
	"draft":                "draft",
	"public draft":         "draft",
	"public-draft":         "draft",
	"initial public draft": "draft",
	"ipd":                  "draft",
	"fpd":                  "draft",
	"withdrawn":            "withdrawn",
	"retired":              "withdrawn",
	"superseded":           "superseded",
	"replaced":             "superseded",
}

func normalizeStatus(explicit string, fallbackText string) (string, bool) {
	cleaned := strings.ToLower(strings.TrimSpace(explicit))
	if cleaned != "" {
		st, ok := statusAliases[cleaned]
		return st, ok
	}
	lower := strings.ToLower(fallbackText)
	switch {
	case strings.Contains(lower, "withdrawn"):
		return "withdrawn", true
	case strings.Contains(lower, "superseded"):
		return "superseded", true
	case strings.Contains(lower, "draft"):
		return "draft", true
	default:
		return "final", true
	}
}
