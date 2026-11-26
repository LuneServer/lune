package store

import (
	"strings"
	"unicode"
)

// buildFTSQuery turns arbitrary user search input into a safe FTS5 MATCH expression
// using prefix matching per token (e.g. `the exp` -> `"the"* "exp"*`).
// Returns "" if the input has no indexable alphanumeric tokens.
func buildFTSQuery(raw string) string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, `"`+cur.String()+`"*`)
			cur.Reset()
		}
	}
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " ")
}
