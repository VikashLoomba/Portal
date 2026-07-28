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
	// The secret is a QUOTED ARGUMENT of the command line, not a reply to
	// security's password prompt: in -i mode that prompt consumes the rest of
	// the (empty) command line, agrees with itself on an empty password, and
	// stores that instead of the real secret.
	wantStdin := []byte("add-generic-password -U -s portal-cred -a \"staging \\\"admin\\\"\\\\db\" -w \"s3kr3t-vector\"\n")
	if !bytes.Equal(call.stdin, wantStdin) {
		t.Errorf("security interactive stdin = %q, want %q", call.stdin, wantStdin)
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

// REGRESSION: a bare `-w` made security's own prompts consume the empty tail of
// the command line, so it stored an EMPTY password, created the item anyway,
// and then failed parsing the secret as a command. Set must therefore pass the
// secret as a quoted argument — and quote the characters security's parser
// treats specially — so the stored value round-trips byte-exactly.
func TestSetQuotesSecretOnTheCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret string
		want   string
	}{
		{"plain", "hunter2", `"hunter2"`},
		{"spaces", "correct horse battery", `"correct horse battery"`},
		{"double quote", `pa"ss`, `"pa\"ss"`},
		{"backslash", `pa\ss`, `"pa\\ss"`},
		{"both", `a"b\c`, `"a\"b\\c"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCommandRunner{}
			store := newStore(filepath.Join(t.TempDir(), "cred-labels"), fake.run)
			if err := store.Set(context.Background(), "lbl", []byte(tc.secret)); err != nil {
				t.Fatal(err)
			}
			got := string(fake.calls[0].stdin)
			wantSuffix := " -w " + tc.want + "\n"
			if !strings.HasSuffix(got, wantSuffix) {
				t.Fatalf("stdin = %q, want it to end with %q", got, wantSuffix)
			}
			// The prompt-driven form is what stored empty passwords.
			if strings.HasSuffix(got, " -w\n") {
				t.Fatal("command still relies on security's interactive password prompt")
			}
		})
	}
}

func TestSetRejectsEmptySecret(t *testing.T) {
	fake := &fakeCommandRunner{}
	store := newStore(filepath.Join(t.TempDir(), "cred-labels"), fake.run)
	if err := store.Set(context.Background(), "lbl", nil); err == nil {
		t.Fatal("storing an empty secret must fail — Get reports such an item absent")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("security calls = %d, want 0 (rejected before exec)", len(fake.calls))
	}
}

// An item left empty by the pre-fix Set must read as ABSENT, so the next
// request prompts for a real secret instead of serving nothing forever.
func TestGetReportsEmptyItemAsAbsent(t *testing.T) {
	for _, stored := range []string{"", "\n", "\r\n"} {
		fake := &fakeCommandRunner{results: []commandResult{{stdout: []byte(stored)}}}
		store := newStore(filepath.Join(t.TempDir(), "cred-labels"), fake.run)
		secret, found, err := store.Get(context.Background(), "lbl")
		if err != nil {
			t.Fatalf("Get(%q): %v", stored, err)
		}
		if found || len(secret) != 0 {
			t.Fatalf("Get(%q) = (%q, %v), want absent", stored, secret, found)
		}
	}
}
