package linkedtoolwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// MatchURL reports whether raw is a valid HTTP(S) URL eligible for linked-tool
// monitoring when an item's state is "linked".
func MatchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http"
}

var trackerTokens = []string{
	"googletagmanager",
	"google-analytics",
	"gtag/js",
	"analytics.js",
	"tracker.js",
	"telemetry.js",
	"pixel.js",
	"beacon.js",
	"plausible.io",
	"segment.com",
	"mixpanel",
	"hotjar",
	"fullstory",
	"clarity.ms",
	"doubleclick.net",
	"connect.facebook.net",
	"intercom",
	"hs-scripts",
	"hubspot",
	"matomo",
	"posthog",
	"amplitude",
}

var paywallClassTokens = map[string]bool{
	"paywall":           true,
	"paywall-gate":      true,
	"paywall-modal":     true,
	"subscription-wall": true,
	"subscription-gate": true,
	"pricing-gate":      true,
	"pro-upgrade-wall":  true,
}

func isTrackerScript(src string, attrs map[string]string) bool {
	for _, k := range []string{"data-tracker", "data-analytics", "data-telemetry"} {
		if v, ok := attrs[k]; ok && v != "" && !strings.EqualFold(v, "false") {
			return true
		}
	}
	lower := strings.ToLower(src)
	for _, tok := range trackerTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

func classifyScriptSrc(src string, pageOrigin string) (isExternalHost bool, canonicalHash string) {
	trimmed := strings.TrimSpace(src)
	if trimmed == "" {
		return false, ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false, shortHash(trimmed)
	}
	scriptHost := strings.ToLower(u.Hostname())
	originHost := ""
	if pageOrigin != "" {
		if ou, err := url.Parse(pageOrigin); err == nil {
			originHost = strings.ToLower(ou.Hostname())
		}
	}
	if scriptHost != "" {
		if originHost != "" {
			isExternalHost = scriptHost != originHost
		} else {
			// Without an explicit origin, an absolute URL with a host is treated
			// as an external host relative to same-origin relative script paths.
			isExternalHost = u.Scheme == "https" || u.Scheme == "http" || strings.HasPrefix(trimmed, "//")
		}
		norm := scriptHost + u.EscapedPath()
		return isExternalHost, shortHash(norm)
	}
	return false, shortHash(u.EscapedPath())
}

func hasPaywallAttr(tag string, attrs map[string]string) bool {
	if tag == "meta" {
		name := strings.ToLower(attrs["name"])
		prop := strings.ToLower(attrs["property"])
		content := strings.ToLower(strings.TrimSpace(attrs["content"]))
		if name == "paywall" && (content == "true" || content == "required" || content == "locked" || content == "1") {
			return true
		}
		if prop == "article:content_tier" && (content == "locked" || content == "metered") {
			return true
		}
	}
	for _, k := range []string{"data-paywall", "data-subscription-required"} {
		if v := strings.ToLower(strings.TrimSpace(attrs[k])); v == "true" || v == "required" || v == "locked" || v == "1" {
			return true
		}
	}
	if cls := attrs["class"]; cls != "" {
		for _, tok := range strings.Fields(strings.ToLower(cls)) {
			if paywallClassTokens[tok] {
				return true
			}
		}
	}
	return false
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
