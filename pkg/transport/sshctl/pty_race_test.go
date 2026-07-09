package sshctl

import (
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/transport/ptyx"
)

func TestPtySessionResizeConcurrentWithWait(t *testing.T) {
	const (
		iterations = 50
		workers    = 4
	)
	for i := 0; i < iterations; i++ {
		cmd := exec.Command("true")
		master, err := ptyx.Start(cmd, 24, 80)
		if err != nil {
			t.Fatalf("iteration %d start pty: %v", i, err)
		}
		sess := &sshctlPtySession{
			master:   master,
			cmd:      cmd,
			waitDone: make(chan struct{}),
		}

		start := make(chan struct{})
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var resizeErrs []error
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-start
				rows := uint16(24 + worker)
				cols := uint16(80 + worker)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := sess.Resize(rows, cols); err != nil {
						mu.Lock()
						resizeErrs = append(resizeErrs, err)
						mu.Unlock()
					}
					runtime.Gosched()
				}
			}(worker)
		}

		waitDone := make(chan error, 1)
		close(start)
		go func() { waitDone <- sess.Wait() }()

		var waitErr error
		select {
		case waitErr = <-waitDone:
		case <-time.After(2 * time.Second):
			close(stop)
			_ = sess.Close()
			wg.Wait()
			t.Fatalf("iteration %d Wait did not return", i)
		}
		close(stop)
		wg.Wait()
		if waitErr != nil {
			t.Fatalf("iteration %d Wait: %v", i, waitErr)
		}

		mu.Lock()
		errs := append([]error(nil), resizeErrs...)
		mu.Unlock()
		for _, err := range errs {
			assertSSHCtlResizeSessionError(t, err)
		}
		if err := sess.Resize(40, 100); err == nil || err.Error() != "sshctl: resize pty after session ended" {
			t.Fatalf("iteration %d Resize after Wait = %v, want session-ended error", i, err)
		}
	}
}

func assertSSHCtlResizeSessionError(t *testing.T, err error) {
	t.Helper()
	switch err.Error() {
	case "sshctl: resize pty after session ended", "sshctl: resize pty after session closed":
	default:
		t.Fatalf("Resize returned unsynchronized pty error %q, want session-ended or session-closed sentinel", err.Error())
	}
}
