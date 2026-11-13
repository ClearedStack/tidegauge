// Package watch holds what every Tidegauge sub-tool shares: the three public
// error codes and the outcome names a check can end with.
package watch

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
