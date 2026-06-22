package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// chooseCtlSock mirrors the socket-selection decision in runSSHProxy so the
// host-matching logic can be tested without spawning ssh. Keep in sync with
// runSSHProxy.
func chooseCtlSock(configuredHost, host, daemonSock string) (sock, persist string, sessionLocal bool) {
	if configuredHost != "" && host == configuredHost {
		return daemonSock, "yes", false
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("portal-ssh-%d.sock", os.Getpid())), "no", true
}

func TestCtlSockSelection(t *testing.T) {
	daemon := "/Users/me/.ssh/cm-portal.sock"

	// Configured dev box → reuse daemon socket, persist, not session-local.
	sock, persist, local := chooseCtlSock("clementine", "clementine", daemon)
	if sock != daemon || persist != "yes" || local {
		t.Errorf("configured host: got (%q,%q,%v), want (%q,yes,false)", sock, persist, local, daemon)
	}

	// Different host → session-local socket, no persist.
	sock, persist, local = chooseCtlSock("clementine", "otherbox", daemon)
	if sock == daemon || persist != "no" || !local {
		t.Errorf("other host: got (%q,%q,%v), want (session-local,no,true)", sock, persist, local)
	}

	// No configured host at all → session-local even if names coincide-ish.
	sock, persist, local = chooseCtlSock("", "clementine", daemon)
	if sock == daemon || persist != "no" || !local {
		t.Errorf("no configured host: got (%q,%q,%v), want (session-local,no,true)", sock, persist, local)
	}
}
