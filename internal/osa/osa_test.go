package osa

import "testing"

func TestStringLiteral(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "portal", want: `"portal"`},
		{name: "quote and backslash", in: `db "root"\admin`, want: `"db \"root\"\\admin"`},
		{name: "control bytes stripped", in: "a\x00b\nc\td\x1f\x7fe", want: `"abcde"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StringLiteral(tt.in); got != tt.want {
				t.Fatalf("StringLiteral(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringLiteralEscapesExistingBackslashBeforeQuote(t *testing.T) {
	// The input is one existing backslash followed by a quote. Escaping the
	// backslash first yields two, then quote protection introduces the third.
	// Reversing those operations would double the quote-protecting backslash.
	if got, want := StringLiteral(`\"`), `"\\\""`; got != want {
		t.Fatalf("StringLiteral ordering result = %q, want %q", got, want)
	}
}
