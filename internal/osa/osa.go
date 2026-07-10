// Package osa renders untrusted display text as safe AppleScript literals.
package osa

import "strings"

// StringLiteral renders s as one double-quoted AppleScript string literal.
// Backslashes and quotes are escaped in AppleScript's own syntax, and control
// bytes are removed so untrusted text cannot terminate the literal or script
// line. Callers that need visual line breaks compose fixed `return` tokens
// between separately escaped literals.
func StringLiteral(s string) string {
	var clean strings.Builder
	clean.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		clean.WriteRune(r)
	}
	s = clean.String()
	// Order matters: escape existing backslashes before introducing the ones
	// that protect double quotes.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
