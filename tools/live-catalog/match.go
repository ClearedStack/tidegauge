package livecatalog

import (
	"net/url"
	"regexp"
	"strings"
)

// MatchURL reports whether raw identifies a ClearedStack live tool or catalog endpoint.
func MatchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "clearedstack.com" || strings.HasSuffix(host, ".clearedstack.com") {
		return true
	}
	return strings.HasPrefix(u.Path, "/tools/") || strings.HasPrefix(u.Path, "/catalog/") || strings.HasPrefix(u.Path, "/live-catalog/")
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validSlug(s string) bool {
	return slugRE.MatchString(strings.TrimSpace(s))
}

var knownCatalogStatuses = map[string]string{
	"live":     "live",
	"healthy":  "live",
	"ok":       "live",
	"degraded": "unhealthy",
	"offline":  "unhealthy",
	"down":     "unhealthy",
	"retired":  "retired",
}

var knownRuleStatuses = map[string]string{
	"":           "active",
	"active":     "active",
	"current":    "active",
	"repealed":   "repealed",
	"superseded": "superseded",
	"withdrawn":  "repealed",
}

func normalizeCatalogStatus(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "live", true
	}
	v, ok := knownCatalogStatuses[s]
	return v, ok
}

func normalizeRuleStatus(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	v, ok := knownRuleStatuses[s]
	return v, ok
}
