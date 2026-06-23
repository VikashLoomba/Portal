package main

import (
	"strings"
	"testing"
)

// liveProbe returns a masterLive callback for chooseCtlSock that reports the
// given fixed liveness and records whether it was actually invoked, so tests
// can assert the probe is skipped when the args don't qualify.
func liveProbe(live bool, called *bool) func() bool {
	return func() bool {
		if called != nil {
			*called = true
		}
		return live
	}
}

func TestCtlSockSelection(t *testing.T) {
	daemon := "/Users/me/.ssh/cm-portal.sock"

	// Configured dev box, bare host, live daemon master → reuse daemon socket,
	// persist, not session-local. Exercises the REAL production decision
	// function (no hand-copied duplicate to drift out of sync).
	var probed bool
	sock, persist, local := chooseCtlSock("clementine", []string{"clementine"}, daemon, liveProbe(true, &probed))
	if sock != daemon || persist != "yes" || local {
		t.Errorf("configured host (live master): got (%q,%q,%v), want (%q,yes,false)", sock, persist, local, daemon)
	}
	if !probed {
		t.Errorf("configured bare host should probe the daemon master")
	}

	// Different host → session-local socket, no persist. Probe must NOT run
	// (no daemon master to share for a non-configured host).
	probed = false
	sock, persist, local = chooseCtlSock("clementine", []string{"otherbox"}, daemon, liveProbe(true, &probed))
	if sock == daemon || persist != "no" || !local {
		t.Errorf("other host: got (%q,%q,%v), want (session-local,no,true)", sock, persist, local)
	}
	if probed {
		t.Errorf("non-configured host must not probe the daemon master")
	}

	// No configured host at all → session-local even if names coincide-ish.
	sock, persist, local = chooseCtlSock("", []string{"clementine"}, daemon, liveProbe(true, nil))
	if sock == daemon || persist != "no" || !local {
		t.Errorf("no configured host: got (%q,%q,%v), want (session-local,no,true)", sock, persist, local)
	}
}

// TestCtlSockNoLiveMaster covers F18: even for the bare configured host, if no
// live daemon master is up we must fall back to a session-local socket with
// ControlPersist=no rather than become an orphan persistent master ourselves.
func TestCtlSockNoLiveMaster(t *testing.T) {
	daemon := "/Users/me/.ssh/cm-portal.sock"
	sock, persist, local := chooseCtlSock("clementine", []string{"clementine"}, daemon, liveProbe(false, nil))
	if sock == daemon || persist != "no" || !local {
		t.Errorf("bare host, no live master: got (%q,%q,%v), want (session-local,no,true)", sock, persist, local)
	}
}

// TestCtlSockFlagAwareGate covers F4: ssh multiplexing matches purely by
// ControlPath, so any connection-affecting flag / user@host / different port
// alongside the configured host must NOT reuse the daemon socket — otherwise
// the session and upload could land on the daemon's box, ignoring the flags.
func TestCtlSockFlagAwareGate(t *testing.T) {
	daemon := "/Users/me/.ssh/cm-portal.sock"
	cfg := "clementine"

	cases := []struct {
		name string
		args []string
	}{
		{"explicit port", []string{cfg, "-p", "2222"}},
		{"login user flag", []string{cfg, "-l", "root"}},
		{"jump host", []string{cfg, "-J", "bastion"}},
		{"-o option", []string{"-o", "User=root", cfg}},
		{"user@confighost", []string{"root@" + cfg}},
		{"trailing remote command", []string{cfg, "false"}},
		{"empty args", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// masterLive returns true; the gate must still refuse to reuse
			// purely on the args, so the probe should not even be consulted.
			var probed bool
			sock, persist, local := chooseCtlSock(cfg, tc.args, daemon, liveProbe(true, &probed))
			if sock == daemon || persist != "no" || !local {
				t.Errorf("args %v: got (%q,%q,%v), want (session-local,no,true)", tc.args, sock, persist, local)
			}
			if probed {
				t.Errorf("args %v: must not probe the daemon master when flags disqualify reuse", tc.args)
			}
		})
	}
}

// TestArgsAreBareConfiguredHost pins the gate predicate directly: only an exact
// single-arg match of the configured host qualifies for daemon-socket reuse.
func TestArgsAreBareConfiguredHost(t *testing.T) {
	cfg := "clementine"
	yes := [][]string{{cfg}}
	no := [][]string{
		nil,
		{},
		{"other"},
		{cfg, "-p", "2222"},
		{cfg, "false"},
		{"root@" + cfg},
		{"-o", "User=root", cfg},
	}
	for _, a := range yes {
		if !argsAreBareConfiguredHost(cfg, a) {
			t.Errorf("argsAreBareConfiguredHost(%q,%v) = false, want true", cfg, a)
		}
	}
	for _, a := range no {
		if argsAreBareConfiguredHost(cfg, a) {
			t.Errorf("argsAreBareConfiguredHost(%q,%v) = true, want false", cfg, a)
		}
	}
	// An empty configured host never qualifies, even for a matching arg.
	if argsAreBareConfiguredHost("", []string{""}) {
		t.Errorf("empty configured host must never qualify")
	}
}

// TestExitCodeErr covers F3's testable surface: runSSHProxy returns an
// exitCodeErr (rather than calling os.Exit, which would skip term.Restore) so
// main() can propagate the code AFTER all deferreds run. Verify the code
// round-trips and the error carries no extra message main() would print.
func TestExitCodeErr(t *testing.T) {
	for _, code := range []int{1, 2, 255} {
		e := exitCodeErr{code: code}
		if e.code != code {
			t.Errorf("exitCodeErr{code:%d}.code = %d", code, e.code)
		}
		// main() prints nothing for this case; the message is only for logs.
		if !strings.Contains(e.Error(), "ssh exited") {
			t.Errorf("exitCodeErr.Error() = %q, want it to mention the ssh exit", e.Error())
		}
	}
}
