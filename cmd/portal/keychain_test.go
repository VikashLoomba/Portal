package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
)

type fakeRememberedCredentialStore struct {
	labels    []string
	listErr   error
	deleteErr error
	deleted   []string
}

func (f *fakeRememberedCredentialStore) List() ([]string, error) {
	return append([]string(nil), f.labels...), f.listErr
}

func (f *fakeRememberedCredentialStore) Delete(_ context.Context, label string) error {
	f.deleted = append(f.deleted, label)
	return f.deleteErr
}

func executeMacKeychainCommand(t *testing.T, deps keychainCommandDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newKeychainCmdWithDeps(deps)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestKeychainListSortedAndEmpty(t *testing.T) {
	t.Run("sorted", func(t *testing.T) {
		store := &fakeRememberedCredentialStore{labels: []string{"zeta", "alpha", "database"}}
		stdout, stderr, err := executeMacKeychainCommand(t, keychainCommandDeps{
			openStore: func() (rememberedCredentialStore, error) { return store, nil },
		}, "list")
		if err != nil {
			t.Fatalf("keychain list: %v", err)
		}
		if want := "alpha\ndatabase\nzeta\n"; stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("empty", func(t *testing.T) {
		store := &fakeRememberedCredentialStore{}
		stdout, stderr, err := executeMacKeychainCommand(t, keychainCommandDeps{
			openStore: func() (rememberedCredentialStore, error) { return store, nil },
		}, "list")
		if err != nil {
			t.Fatalf("keychain list: %v", err)
		}
		if stdout != "" || stderr != "" {
			t.Fatalf("empty list streams = stdout %q, stderr %q", stdout, stderr)
		}
	})
}

func TestKeychainForgetDeletesAuditsAndConfirms(t *testing.T) {
	store := &fakeRememberedCredentialStore{}
	auditLog := audit.New(t.TempDir())
	stdout, stderr, err := executeMacKeychainCommand(t, keychainCommandDeps{
		openStore: func() (rememberedCredentialStore, error) { return store, nil },
		audit:     auditLog,
		host:      "devbox",
	}, "forget", "staging admin")
	if err != nil {
		t.Fatalf("keychain forget: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "staging admin" {
		t.Fatalf("deleted labels = %q, want staging admin", store.deleted)
	}
	if want := "forgotten credential \"staging admin\"\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	assertCredAudit(t, auditLog, []string{
		"cred-forgotten", "host=devbox", "label=staging admin",
	})
}

func TestKeychainForgetFailureDoesNotConfirmOrAudit(t *testing.T) {
	store := &fakeRememberedCredentialStore{deleteErr: errors.New("locked")}
	auditLog := audit.New(t.TempDir())
	stdout, _, err := executeMacKeychainCommand(t, keychainCommandDeps{
		openStore: func() (rememberedCredentialStore, error) { return store, nil },
		audit:     auditLog,
		host:      "devbox",
	}, "forget", "database")
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("error = %v, want locked failure", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no confirmation", stdout)
	}
	if events := credAuditFieldsIfPresent(t, auditLog); len(events) != 0 {
		t.Fatalf("audit events = %q, want none", events)
	}
}

func TestKeychainUsageErrorsBeforeOpeningStore(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"list extra", []string{"list", "extra"}, "usage: portal keychain list"},
		{"forget missing", []string{"forget"}, "usage: portal keychain forget <label>"},
		{"forget extra", []string{"forget", "one", "two"}, "usage: portal keychain forget <label>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opened := 0
			_, _, err := executeMacKeychainCommand(t, keychainCommandDeps{
				openStore: func() (rememberedCredentialStore, error) {
					opened++
					return &fakeRememberedCredentialStore{}, nil
				},
			}, tt.args...)
			var usage usageErr
			if !errors.As(err, &usage) {
				t.Fatalf("error = %v, want usageErr", err)
			}
			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
			if opened != 0 {
				t.Fatalf("store opened %d times before usage validation", opened)
			}
		})
	}
}

func TestKeychainNoSubcommandShowsMacDevBoxSplit(t *testing.T) {
	stdout, stderr, err := executeMacKeychainCommand(t, keychainCommandDeps{})
	if err != nil {
		t.Fatalf("keychain help: %v", err)
	}
	for _, want := range []string{
		"remembered by portal in the macOS Keychain on THIS Mac",
		"portal keychain list",
		"portal keychain forget <label>",
		"dev-box-only",
		"portal keychain run ...",
		"portal keychain askpass ...",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRootHelpRegistersAndReferencesKeychain(t *testing.T) {
	a := &app.App{Cfg: newTestConfig(t, "devbox")}
	root := newRootCmd(a)
	cmd, _, err := root.Find([]string{"keychain"})
	if err != nil {
		t.Fatalf("root keychain lookup: %v", err)
	}
	if cmd == nil || cmd.Name() != "keychain" {
		t.Fatalf("root keychain lookup = %v, want keychain command", cmd)
	}
	help := helpText(a)
	for _, want := range []string{
		"Credentials",
		"keychain list",
		"keychain forget <label>",
		"portal keychain run ...",
		"cred gates",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("root help missing %q:\n%s", want, help)
		}
	}
}

func credAuditFieldsIfPresent(t *testing.T, log *audit.Log) [][]string {
	t.Helper()
	if _, err := os.Stat(log.Path()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return credAuditFields(t, log)
}
