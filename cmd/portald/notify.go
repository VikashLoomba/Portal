package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/VikashLoomba/Portal/internal/notify"
)

// notifyReadLimit bounds the hook JSON read from stdin. Claude Code hook
// payloads are small; this guards against a pathological producer streaming an
// unbounded body into the agent. It is larger than the agent's notifyBodyMax
// because the raw hook JSON (which we classify down) can carry a long
// last_assistant_message we then truncate to the notification body.
const notifyReadLimit = 256 << 10 // 256 KiB

// notifyDialTimeout / notifyReadTimeout bound the cmd-socket round trip. A hook
// runs synchronously in Claude Code's process, so keep these tight — a missed
// notification must never stall the coding agent.
const (
	notifyDialTimeout = 2 * time.Second
	notifyReadTimeout = 4 * time.Second
)

// notifyPost is the JSON the agent's `notify` cmd-socket verb parses. It is the
// already-classified notification; the structured-hook-vs-generic split (which
// sets Verified) happens HERE on the remote side, mirroring cc-clip's
// Content-Type branch on its /notify endpoint.
type notifyPost struct {
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Urgency  uint8  `json:"urgency,omitempty"`
	Verified bool   `json:"verified"`
	Source   string `json:"source,omitempty"`
	Sound    string `json:"sound,omitempty"`
}

// runNotify implements `portald notify`. Two modes:
//
//	--hook              Read a Claude Code hook JSON object on stdin, classify it
//	                    via internal/notify, and post it VERIFIED (it arrived via
//	                    the structured hook entrypoint installed by portal).
//	--title T [--body B] Post a generic notification UNVERIFIED (the Mac renders
//	[--urgency N] [--sound S]  it with an "[unverified] " prefix).
//
// It exits 0 only if exactly one connected client accepted the relay; 1
// otherwise (no client, malformed payload, dial failure, channel full).
func runNotify(args []string) {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		hook     = fs.Bool("hook", false, "read a Claude Code hook JSON object on stdin (verified)")
		title    = fs.String("title", "", "notification title (generic mode)")
		body     = fs.String("body", "", "notification body (generic mode)")
		subtitle = fs.String("subtitle", "", "notification subtitle (generic mode)")
		urgency  = fs.Uint("urgency", 1, "urgency tier 0..2 (generic mode)")
		sound    = fs.String("sound", "", "macOS sound name (generic mode)")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	var post notifyPost
	if *hook {
		p, ok := classifyHookStdin()
		if !ok {
			os.Exit(1)
		}
		post = p
	} else {
		if *title == "" {
			fmt.Fprintln(os.Stderr, "usage: portald notify --hook | --title <t> [--body <b>] [--subtitle <s>] [--urgency 0..2] [--sound <s>]")
			os.Exit(1)
		}
		post = notifyPost{
			Title:    *title,
			Body:     *body,
			Subtitle: *subtitle,
			Urgency:  uint8(*urgency),
			Verified: false, // generic callers are never verified
			Source:   "generic",
			Sound:    *sound,
		}
	}

	payload, err := json.Marshal(post)
	if err != nil {
		os.Exit(1)
	}
	if !notifyFanout(string(payload)) {
		os.Exit(1)
	}
}

// classifyHookStdin reads the hook JSON object from stdin (bounded) and maps it
// to a VERIFIED notifyPost via internal/notify.ClassifyHook. Claude Code passes
// the event name in "hook_event_name"; we also accept "hookType" as a fallback.
// Returns ok=false on a read/parse error so the caller exits 1.
func classifyHookStdin() (notifyPost, bool) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, notifyReadLimit))
	if err != nil || len(raw) == 0 {
		return notifyPost{}, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return notifyPost{}, false
	}
	hookEvent := mapStr(obj, "hook_event_name")
	if hookEvent == "" {
		hookEvent = mapStr(obj, "hookType")
	}
	c := notify.ClassifyHook(hookEvent, obj)
	return notifyPost{
		Title:    c.Title,
		Body:     c.Body,
		Urgency:  c.Urgency,
		Verified: true, // arrived via the structured hook entrypoint
		Source:   c.Source,
	}, true
}

func mapStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// notifyFanout dials every cmd-*.sock and posts the `notify\t<json>` line,
// mirroring runOpen's fanout. It returns true if at least one connected client
// accepted the relay ("ok"). UNLIKE clipFanout it does NOT refuse on multiple
// connected clients: a notification is broadcast-safe (every connected Mac may
// reasonably want it), and there is no clipboard-content cross-leak risk.
func notifyFanout(jsonBody string) bool {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "portal")
	if self, err := os.Executable(); err == nil {
		dir = filepath.Dir(self)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "cmd-*.sock"))
	if len(entries) == 0 {
		return false
	}

	line := "notify\t" + jsonBody + "\n"
	accepted := false
	for _, sock := range entries {
		conn, err := net.DialTimeout("unix", sock, notifyDialTimeout)
		if err != nil {
			continue // stale socket / agent gone
		}
		conn.SetDeadline(time.Now().Add(notifyReadTimeout))
		_, _ = io.WriteString(conn, line)
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		conn.Close()
		if trimReply(string(buf[:n])) == "ok" {
			accepted = true
		}
	}
	return accepted
}

// trimReply trims a one-line cmd-socket reply to its bare verb.
func trimReply(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\r' || raw[i] == '\n' {
			return raw[:i]
		}
	}
	return raw
}
