package main

import "testing"

// TestAppleScriptStr verifies the AppleScript-injection sanitizer escapes the
// two metacharacters that matter inside a double-quoted AppleScript string
// literal (backslash and double-quote) and strips control bytes — so a hostile
// remote notification title/body cannot break out of the `display notification`
// literal and run arbitrary AppleScript.
func TestAppleScriptStr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", `"hello world"`},
		{"double quote", `say "hi"`, `"say \"hi\""`},
		{"backslash", `a\b`, `"a\\b"`},
		{"backslash then quote", `a\"b`, `"a\\\"b"`},
		{"newline stripped", "line1\nline2", `"line1line2"`},
		{"carriage return stripped", "a\rb", `"ab"`},
		{"tab stripped", "a\tb", `"ab"`},
		{"nul stripped", "a\x00b", `"ab"`},
		{
			// The classic injection: closing the string then appending an
			// AppleScript verb. After escaping, the inner quote is neutralized
			// and the whole thing stays one literal.
			name: "injection attempt neutralized",
			in:   `x" & (do shell script "rm -rf ~") & "`,
			want: `"x\" & (do shell script \"rm -rf ~\") & \""`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleScriptStr(tc.in); got != tc.want {
				t.Errorf("appleScriptStr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripControl ensures every ASCII control byte (and DEL) is removed while
// printable / multibyte runes survive.
func TestStripControl(t *testing.T) {
	if got := stripControl("a\x01\x1f\x7fb"); got != "ab" {
		t.Errorf("stripControl = %q, want %q", got, "ab")
	}
	if got := stripControl("héllo ✓"); got != "héllo ✓" {
		t.Errorf("stripControl mangled printable/multibyte: %q", got)
	}
}

// TestDefaultSoundForUrgency verifies only the critical tier chimes by default.
func TestDefaultSoundForUrgency(t *testing.T) {
	if s := defaultSoundForUrgency(2); s == "" {
		t.Error("critical urgency should have a default sound")
	}
	if s := defaultSoundForUrgency(1); s != "" {
		t.Errorf("attention urgency should be silent by default, got %q", s)
	}
	if s := defaultSoundForUrgency(0); s != "" {
		t.Errorf("calm urgency should be silent by default, got %q", s)
	}
}
