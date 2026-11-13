package ecfr

import (
	"encoding/xml"
	"strings"
)

// The matching rule: what this sub-tool accepts as an eCFR document.
//
// The eCFR publishes a title, or any piece of one, as XML whose root is a
// division element named DIV1 to DIV9. Every division carries a TYPE
// attribute naming its level and an N attribute holding its number. A
// document is matched when its root is such a division. Anything else that
// is well-formed XML is valid and unfamiliar: E_NEEDS_SUBTOOL.

// divisionTypes are the levels this sub-tool knows.
var divisionTypes = map[string]bool{
	"TITLE":    true,
	"SUBTITLE": true,
	"CHAPTER":  true,
	"SUBCHAP":  true,
	"PART":     true,
	"SUBPART":  true,
	"SUBJGRP":  true,
	"SECTION":  true,
	"APPENDIX": true,
}

// isDivision reports whether an element is named DIV1 to DIV9.
func isDivision(name xml.Name) bool {
	l := name.Local
	return name.Space == "" && len(l) == 4 && strings.HasPrefix(l, "DIV") && l[3] >= '1' && l[3] <= '9'
}

// divisionLabel returns "TYPE N" for a division this sub-tool recognises,
// for example "SECTION 901.102". ok is false when the element is not a
// division, its TYPE is not a known level, or it has no N.
func divisionLabel(el xml.StartElement) (label string, ok bool) {
	if !isDivision(el.Name) {
		return "", false
	}
	var typ, n string
	for _, a := range el.Attr {
		if a.Name.Space != "" {
			continue
		}
		switch a.Name.Local {
		case "TYPE":
			typ = a.Value
		case "N":
			n = normalise(a.Value)
		}
	}
	if !divisionTypes[typ] || n == "" {
		return "", false
	}
	return typ + " " + n, true
}
