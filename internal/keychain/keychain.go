// Package keychain stores remembered portal credentials in the macOS Keychain
// and maintains a labels-only index for listing and forgetting them.
package keychain

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	securityBinary    = "/usr/bin/security"
	credentialService = "portal-cred"
	notFoundExitCode  = 44
)

var errSecurityUnavailable = errors.New("keychain: macOS security is unavailable")

type commandResult struct {
	stdout   []byte
	exitCode int
	err      error
}

type commandRunner func(context.Context, string, []string, []byte) commandResult

// Store persists credentials under the portal-cred service and tracks their
// account labels in ~/.config/portal/cred-labels. Construct one with New.
type Store struct {
	mu        sync.Mutex
	indexPath string
	run       commandRunner
}

// New returns the current user's credential Store.
func New() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("keychain: locate home directory: %w", err)
	}
	return newStore(filepath.Join(home, ".config", "portal", "cred-labels"), defaultCommandRunner), nil
}

func newStore(indexPath string, runner commandRunner) *Store {
	return &Store{indexPath: indexPath, run: runner}
}

// Set adds or updates label's Keychain item, then adds label to the local
// index. The password is supplied to security's interactive -w prompt on stdin
// and never appears in a process argument.
func (s *Store) Set(ctx context.Context, label string, secret []byte) error {
	quotedLabel, err := securityCommandArg(label)
	if err != nil {
		return err
	}
	if bytes.IndexAny(secret, "\r\n\x00") >= 0 {
		return errors.New("keychain: remembered secret contains an unsupported control byte")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	command := "add-generic-password -U -s " + credentialService + " -a " + quotedLabel + " -w\n"
	stdin := make([]byte, 0, len(command)+len(secret)+1)
	stdin = append(stdin, command...)
	stdin = append(stdin, secret...)
	stdin = append(stdin, '\n')
	result := s.run(ctx, securityBinary, []string{"-i"}, stdin)
	clear(stdin)
	if err := commandError(ctx, "add", result); err != nil {
		return err
	}
	return s.addLabelLocked(label)
}

// Get returns label's secret and whether a matching Keychain item exists.
// Keychain exit status 44 is treated as an ordinary absent item.
func (s *Store) Get(ctx context.Context, label string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.run(ctx, securityBinary,
		[]string{"find-generic-password", "-s", credentialService, "-a", label, "-w"}, nil)
	if result.exitCode == notFoundExitCode {
		return nil, false, nil
	}
	if err := commandError(ctx, "find", result); err != nil {
		return nil, false, err
	}
	secret := append([]byte(nil), result.stdout...)
	if len(secret) > 0 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
		if len(secret) > 0 && secret[len(secret)-1] == '\r' {
			secret = secret[:len(secret)-1]
		}
	}
	return secret, true, nil
}

// Delete removes label's Keychain item and index entry. A missing Keychain item
// or missing index is tolerated so external Keychain Access edits cannot wedge
// cleanup.
func (s *Store) Delete(ctx context.Context, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.run(ctx, securityBinary,
		[]string{"delete-generic-password", "-s", credentialService, "-a", label}, nil)
	if result.exitCode != notFoundExitCode {
		if err := commandError(ctx, "delete", result); err != nil {
			return err
		}
	}
	return s.removeLabelLocked(label)
}

// List returns the sorted labels index. A missing index is an empty list; it is
// not reconciled against Keychain so drift remains harmless and inexpensive.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLabelsLocked()
}

func commandError(ctx context.Context, operation string, result commandResult) error {
	if result.err == nil && result.exitCode == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("keychain: security %s failed", operation)
}

func securityCommandArg(value string) (string, error) {
	if value == "" {
		return "", errors.New("keychain: label is empty")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("keychain: label contains an unsupported control byte")
		}
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`, nil
}

func (s *Store) addLabelLocked(label string) error {
	labels, err := s.readLabelsLocked()
	if err != nil {
		return err
	}
	for _, existing := range labels {
		if existing == label {
			return nil
		}
	}
	labels = append(labels, label)
	return s.writeLabelsLocked(labels)
}

func (s *Store) removeLabelLocked(label string) error {
	labels, err := s.readLabelsLocked()
	if err != nil {
		return err
	}
	kept := labels[:0]
	for _, existing := range labels {
		if existing != label {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(labels) {
		return nil
	}
	return s.writeLabelsLocked(kept)
}

func (s *Store) readLabelsLocked() ([]string, error) {
	f, err := os.Open(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]struct{})
	labels := make([]string, 0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		label := strings.TrimSuffix(scanner.Text(), "\r")
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Strings(labels)
	return labels, nil
}

func (s *Store) writeLabelsLocked(labels []string) error {
	labels = append([]string(nil), labels...)
	sort.Strings(labels)
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.indexPath), ".cred-labels-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	for _, label := range labels {
		if _, err := fmt.Fprintln(tmp, label); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.indexPath); err != nil {
		return err
	}
	removeTemp = false
	return os.Chmod(s.indexPath, 0o600)
}
