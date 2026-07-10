package keychain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commandCall struct {
	path  string
	args  []string
	stdin []byte
}

type fakeCommandRunner struct {
	results []commandResult
	calls   []commandCall
}

func (f *fakeCommandRunner) run(_ context.Context, path string, args []string, stdin []byte) commandResult {
	f.calls = append(f.calls, commandCall{
		path:  path,
		args:  append([]string(nil), args...),
		stdin: append([]byte(nil), stdin...),
	})
	if len(f.results) == 0 {
		return commandResult{}
	}
	result := f.results[0]
	f.results = f.results[1:]
	result.stdout = append([]byte(nil), result.stdout...)
	return result
}

func TestSetUsesInteractiveStdinAndIndexesLabel(t *testing.T) {
	fake := &fakeCommandRunner{}
	indexPath := filepath.Join(t.TempDir(), "portal", "cred-labels")
	store := newStore(indexPath, fake.run)
	label := `staging "admin"\db`
	secret := []byte("s3kr3t-vector")
	if err := store.Set(context.Background(), label, secret); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("security calls = %d, want 1", len(fake.calls))
	}
	call := fake.calls[0]
	if call.path != securityBinary {
		t.Errorf("path = %q, want %q", call.path, securityBinary)
	}
	if !reflect.DeepEqual(call.args, []string{"-i"}) {
		t.Errorf("argv = %q, want [-i]", call.args)
	}
	for _, token := range append([]string{call.path}, call.args...) {
		if bytes.Contains([]byte(token), secret) {
			t.Fatalf("secret appeared in argv token")
		}
	}
	wantStdin := []byte("add-generic-password -U -s portal-cred -a \"staging \\\"admin\\\"\\\\db\" -w\n")
	wantStdin = append(wantStdin, secret...)
	wantStdin = append(wantStdin, '\n')
	if !bytes.Equal(call.stdin, wantStdin) {
		t.Errorf("security interactive stdin shape differs")
	}
	stdinCommand := string(call.stdin[:bytes.IndexByte(call.stdin, '\n')])
	for _, want := range []string{"add-generic-password", "-U", "-s portal-cred", "-w"} {
		if !strings.Contains(stdinCommand, want) {
			t.Errorf("interactive command missing %q", want)
		}
	}
	if strings.Contains(stdinCommand, "-T") {
		t.Fatalf("interactive command unexpectedly contains -T: %s", stdinCommand)
	}

	labels, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(labels, []string{label}) {
		t.Errorf("List = %q, want [%q]", labels, label)
	}
	info, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("index mode = %o, want 600", got)
	}
}

func TestGetParsesPasswordAndNotFound(t *testing.T) {
	fake := &fakeCommandRunner{results: []commandResult{
		{stdout: []byte("read-back\r\n")},
		{exitCode: notFoundExitCode, err: errors.New("exit status 44")},
		{exitCode: 1, err: errors.New("exit status 1")},
	}}
	store := newStore(filepath.Join(t.TempDir(), "cred-labels"), fake.run)

	secret, found, err := store.Get(context.Background(), "database")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !bytes.Equal(secret, []byte("read-back")) {
		t.Errorf("Get success: found=%v, secret length=%d", found, len(secret))
	}
	wantArgs := []string{"find-generic-password", "-s", "portal-cred", "-a", "database", "-w"}
	if !reflect.DeepEqual(fake.calls[0].args, wantArgs) {
		t.Errorf("find argv = %q, want %q", fake.calls[0].args, wantArgs)
	}

	secret, found, err = store.Get(context.Background(), "missing")
	if err != nil || found || secret != nil {
		t.Errorf("Get missing: found=%v, secret length=%d, err=%v", found, len(secret), err)
	}
	if _, _, err := store.Get(context.Background(), "broken"); err == nil {
		t.Fatal("Get non-44 security failure returned nil error")
	}
}

func TestDeleteToleratesNotFoundAndRemovesIndex(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "cred-labels")
	if err := os.WriteFile(indexPath, []byte("orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommandRunner{results: []commandResult{{
		exitCode: notFoundExitCode,
		err:      errors.New("exit status 44"),
	}}}
	store := newStore(indexPath, fake.run)
	if err := store.Delete(context.Background(), "orphan"); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"delete-generic-password", "-s", "portal-cred", "-a", "orphan"}
	if !reflect.DeepEqual(fake.calls[0].args, wantArgs) {
		t.Errorf("delete argv = %q, want %q", fake.calls[0].args, wantArgs)
	}
	labels, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Errorf("List after Delete = %q, want empty", labels)
	}
}

func TestLabelsIndexRoundTripAndDrift(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "nested", "cred-labels")
	fake := &fakeCommandRunner{}
	store := newStore(indexPath, fake.run)
	labels, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Fatalf("missing index List = %q, want empty", labels)
	}
	for _, label := range []string{"zeta", "alpha", "zeta"} {
		if err := store.Set(context.Background(), label, []byte("remembered")); err != nil {
			t.Fatal(err)
		}
	}
	labels, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(labels, []string{"alpha", "zeta"}) {
		t.Errorf("List = %q, want sorted unique labels", labels)
	}

	// Keychain Access may remove an item without updating the labels-only index.
	// A status-44 lookup is simply absent and leaves that harmless drift intact.
	fake.results = append(fake.results, commandResult{exitCode: notFoundExitCode, err: errors.New("exit status 44")})
	secret, found, err := store.Get(context.Background(), "zeta")
	if err != nil || found || secret != nil {
		t.Errorf("drifted Get: found=%v, secret length=%d, err=%v", found, len(secret), err)
	}
	labels, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(labels, []string{"alpha", "zeta"}) {
		t.Errorf("drifted index changed unexpectedly: %q", labels)
	}
}

func TestSetRejectsInteractiveFramingBytesBeforeExec(t *testing.T) {
	tests := []struct {
		name   string
		label  string
		secret []byte
	}{
		{name: "label newline", label: "bad\nlabel", secret: []byte("safe")},
		{name: "secret newline", label: "safe", secret: []byte("bad\nsecret")},
		{name: "secret nul", label: "safe", secret: []byte{'a', 0, 'b'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCommandRunner{}
			store := newStore(filepath.Join(t.TempDir(), "cred-labels"), fake.run)
			if err := store.Set(context.Background(), tt.label, tt.secret); err == nil {
				t.Fatal("Set returned nil error")
			}
			if len(fake.calls) != 0 {
				t.Fatalf("security invoked %d times for invalid framing", len(fake.calls))
			}
		})
	}
}
