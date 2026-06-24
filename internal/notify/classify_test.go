package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClassifyHook covers the full hook_event_name → {title, urgency} mapping
// ported from cc-clip's ClassifyHookPayload, plus the generic fallback and the
// body-source precedence (body before message; last_assistant_message for stop).
func TestClassifyHook(t *testing.T) {
	tests := []struct {
		name        string
		event       string
		raw         map[string]any
		wantTitle   string
		wantUrgency uint8
		wantBody    string
	}{
		{
			name:        "notification permission_prompt → critical",
			event:       "Notification",
			raw:         map[string]any{"type": "permission_prompt", "message": "Bash wants to run"},
			wantTitle:   "Tool approval needed",
			wantUrgency: UrgencyCritical,
			wantBody:    "Bash wants to run",
		},
		{
			name:        "notification idle_prompt → attention",
			event:       "Notification",
			raw:         map[string]any{"type": "idle_prompt"},
			wantTitle:   "Claude is idle",
			wantUrgency: UrgencyAttention,
		},
		{
			name:        "notification other with title",
			event:       "notification",
			raw:         map[string]any{"type": "weird", "title": "Custom"},
			wantTitle:   "Custom",
			wantUrgency: UrgencyAttention,
		},
		{
			name:        "notification other without title uses type",
			event:       "notification",
			raw:         map[string]any{"type": "weird"},
			wantTitle:   "Claude notification: weird",
			wantUrgency: UrgencyAttention,
		},
		{
			name:        "notification body falls back to message",
			event:       "Notification",
			raw:         map[string]any{"type": "idle_prompt", "message": "msg-fallback"},
			wantTitle:   "Claude is idle",
			wantUrgency: UrgencyAttention,
			wantBody:    "msg-fallback",
		},
		{
			name:        "notification prefers body over message",
			event:       "Notification",
			raw:         map[string]any{"type": "idle_prompt", "body": "the-body", "message": "msg"},
			wantTitle:   "Claude is idle",
			wantUrgency: UrgencyAttention,
			wantBody:    "the-body",
		},
		{
			name:        "stop end-of-turn → calm finished",
			event:       "Stop",
			raw:         map[string]any{"stop_hook_reason": "stop_at_end_of_turn", "last_assistant_message": "done"},
			wantTitle:   "Claude finished",
			wantUrgency: UrgencyCalm,
			wantBody:    "done",
		},
		{
			name:        "stop empty reason → calm finished",
			event:       "stop",
			raw:         map[string]any{"last_assistant_message": "all done"},
			wantTitle:   "Claude finished",
			wantUrgency: UrgencyCalm,
			wantBody:    "all done",
		},
		{
			name:        "stop other reason → attention stopped",
			event:       "stop",
			raw:         map[string]any{"stop_hook_reason": "interrupted"},
			wantTitle:   "Claude stopped",
			wantUrgency: UrgencyAttention,
		},
		{
			name:        "unknown hook event → generic title",
			event:       "SubagentStop",
			raw:         map[string]any{"foo": "bar"},
			wantTitle:   "Claude hook: SubagentStop",
			wantUrgency: UrgencyAttention,
		},
		{
			name:        "empty hook event → generic event title",
			event:       "",
			raw:         map[string]any{},
			wantTitle:   "Claude hook: event",
			wantUrgency: UrgencyAttention,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHook(tc.event, tc.raw)
			if got.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", got.Title, tc.wantTitle)
			}
			if got.Urgency != tc.wantUrgency {
				t.Errorf("urgency = %d, want %d", got.Urgency, tc.wantUrgency)
			}
			if tc.wantBody != "" && got.Body != tc.wantBody {
				t.Errorf("body = %q, want %q", got.Body, tc.wantBody)
			}
			if got.Source != "claude_hook" {
				t.Errorf("source = %q, want claude_hook", got.Source)
			}
		})
	}
}

// TestTruncate verifies the body is capped at bodyMaxRunes with an ellipsis on a
// UTF-8 boundary, and that an exactly-fitting / shorter body is untouched.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 280); got != "short" {
		t.Errorf("short string mangled: %q", got)
	}
	long := strings.Repeat("x", 400)
	got := truncate(long, bodyMaxRunes)
	if utf8.RuneCountInString(got) != bodyMaxRunes+1 { // +1 for the ellipsis
		t.Errorf("truncated len = %d runes, want %d", utf8.RuneCountInString(got), bodyMaxRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated body missing ellipsis: %q", got[len(got)-4:])
	}
	// Multi-byte runes must not be split mid-byte.
	multi := strings.Repeat("é", 400)
	gotM := truncate(multi, bodyMaxRunes)
	if !utf8.ValidString(gotM) {
		t.Errorf("truncate split a multi-byte rune: invalid UTF-8")
	}
}
