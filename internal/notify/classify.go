// Package notify ports cc-clip's hook-payload classifier: it turns a Claude
// Code hook JSON object (read from the hook's stdin) into the {title, body,
// urgency} triple the Mac raises as a native notification. The classifier is
// deliberately transport-free so it can be shared by cmd/portald (the remote
// `portald notify --hook` entrypoint) and exercised in table-driven tests
// without a live socket.
//
// The mapping mirrors cc-clip's ClassifyHookPayload (see DESIGN; the recon spec
// captured it verbatim from the cc-clip source):
//
//	notification / permission_prompt → "Tool approval needed"   urgency 2
//	notification / idle_prompt       → "Claude is idle"         urgency 1
//	notification / <other>           → title or "Claude notification: <type>" urgency 1
//	stop / (end-of-turn)             → "Claude finished"        urgency 0
//	stop / <other>                   → "Claude stopped"         urgency 1
//	<other hook event>               → "Claude hook: <event>"   urgency 1 (generic body)
//
// Codex completion (cc-clip's `~/.codex/config.toml notify`) is OUT OF SCOPE per
// the mission (Codex reads X11/Wayland in-process and cannot be PATH-shim-
// intercepted); only the Claude Code hook surface is ported here.
package notify

import (
	"strings"
	"unicode/utf8"
)

// Urgency tiers. The Mac maps these to an optional notification sound.
const (
	UrgencyCalm      uint8 = 0 // end-of-turn completion
	UrgencyAttention uint8 = 1 // idle / generic notification / non-end stop
	UrgencyCritical  uint8 = 2 // tool-approval / permission prompt
)

// bodyMaxRunes bounds the notification body so a runaway last_assistant_message
// (Claude can emit very long turns) cannot blow up the AppleScript literal.
// Matches cc-clip's 280-rune truncation.
const bodyMaxRunes = 280

// Classified is the transport-free result of inspecting a hook payload.
type Classified struct {
	Title   string
	Body    string
	Urgency uint8
	Source  string // always "claude_hook" for the structured path
}

// ClassifyHook maps a decoded Claude Code hook payload (the JSON object the
// hook receives on stdin) to a Classified notification. hookEvent is the value
// of the "hook_event_name" field (Claude Code) — callers should lower-case
// nothing; we compare case-insensitively here. raw is the rest of the decoded
// object. The result is always populated (never empty title) so the Mac always
// has something to show.
func ClassifyHook(hookEvent string, raw map[string]any) Classified {
	c := Classified{Source: "claude_hook"}
	switch strings.ToLower(hookEvent) {
	case "notification":
		notifType := str(raw, "type")
		body := str(raw, "body")
		if body == "" {
			body = str(raw, "message")
		}
		switch notifType {
		case "permission_prompt":
			c.Title = "Tool approval needed"
			c.Urgency = UrgencyCritical
		case "idle_prompt":
			c.Title = "Claude is idle"
			c.Urgency = UrgencyAttention
		default:
			if t := str(raw, "title"); t != "" {
				c.Title = t
			} else if notifType != "" {
				c.Title = "Claude notification: " + notifType
			} else {
				c.Title = "Claude notification"
			}
			c.Urgency = UrgencyAttention
		}
		c.Body = truncate(body, bodyMaxRunes)

	case "stop":
		reason := str(raw, "stop_hook_reason")
		msg := str(raw, "last_assistant_message")
		if reason == "" || reason == "stop_at_end_of_turn" {
			c.Title = "Claude finished"
			c.Urgency = UrgencyCalm
		} else {
			c.Title = "Claude stopped"
			c.Urgency = UrgencyAttention
		}
		c.Body = truncate(msg, bodyMaxRunes)

	default:
		// Unknown hook event (SubagentStop, PreToolUse, PostToolUse, …): route
		// through a generic title so a future Claude Code hook still surfaces
		// something rather than being silently dropped.
		ev := hookEvent
		if ev == "" {
			ev = "event"
		}
		c.Title = "Claude hook: " + ev
		c.Body = truncate(stringifyMap(raw), bodyMaxRunes)
		c.Urgency = UrgencyAttention
	}
	return c
}

// str returns raw[key] as a string when it is a string, else "".
func str(raw map[string]any, key string) string {
	if v, ok := raw[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// truncate trims s to at most n runes, appending "…" on a UTF-8 boundary when
// it had to cut. Matches cc-clip's truncate semantics.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// stringifyMap renders a generic payload as "key=value" pairs (skipping the
// internal _portal_* injection keys) for the fallback generic-hook body, so an
// unknown hook event still shows its salient fields.
func stringifyMap(raw map[string]any) string {
	var b strings.Builder
	first := true
	for k, v := range raw {
		if strings.HasPrefix(k, "_portal_") {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if !first {
			b.WriteString(" ")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(s)
		first = false
	}
	return b.String()
}
