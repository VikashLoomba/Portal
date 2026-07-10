package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/prompt"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

type fakeCredGetResult struct {
	secret []byte
	found  bool
	err    error
}

type fakeCredSetCall struct {
	label  string
	secret []byte
}

type fakeCredKeychain struct {
	mu         sync.Mutex
	getResults []fakeCredGetResult
	getCalls   []string
	sets       []fakeCredSetCall
	deletes    []string
	setErr     error
	deleteErr  error
}

func (f *fakeCredKeychain) Get(_ context.Context, label string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, label)
	if len(f.getResults) == 0 {
		return nil, false, nil
	}
	result := f.getResults[0]
	f.getResults = f.getResults[1:]
	return append([]byte(nil), result.secret...), result.found, result.err
}

func (f *fakeCredKeychain) Set(_ context.Context, label string, secret []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, fakeCredSetCall{label: label, secret: append([]byte(nil), secret...)})
	return f.setErr
}

func (f *fakeCredKeychain) Delete(_ context.Context, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, label)
	return f.deleteErr
}

type credTestState struct {
	mu      sync.Mutex
	now     time.Time
	enabled bool
	logs    []string
}

func (s *credTestState) nowTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *credTestState) setNow(now time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

func (s *credTestState) featureEnabled(feature string) bool {
	if feature != config.FeatureCred {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *credTestState) setEnabled(enabled bool) {
	s.mu.Lock()
	s.enabled = enabled
	s.mu.Unlock()
}

func (s *credTestState) log(line string) {
	s.mu.Lock()
	s.logs = append(s.logs, line)
	s.mu.Unlock()
}

func (s *credTestState) logLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

func newCredTestDeps(t *testing.T, prompter prompt.Prompter, kc *fakeCredKeychain) (credServeDeps, *credTestState) {
	t.Helper()
	state := &credTestState{
		now:     time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		enabled: true,
	}
	return credServeDeps{
		Prompter:       prompter,
		KC:             kc,
		FeatureEnabled: state.featureEnabled,
		Audit:          audit.New(t.TempDir()),
		Host:           "box",
		Cooldown:       newCredCooldown(),
		Now:            state.nowTime,
		Log:            state.log,
	}, state
}

func baseCredEvent() *agentclient.CredEvent {
	return &agentclient.CredEvent{
		Nonce: 41, Epoch: 7, Label: "database", Requester: "pid 42: sh",
		Mode: "env", Target: "PW",
	}
}

func assertCredResponse(t *testing.T, got, want *protocol.CredResponse) {
	t.Helper()
	if got == nil {
		t.Fatal("serveCredRequest returned nil")
	}
	if got.Nonce != want.Nonce || got.Epoch != want.Epoch || got.OK != want.OK || got.Err != want.Err {
		t.Fatal("credential response metadata differs")
	}
	if (got.Secret == nil) != (want.Secret == nil) || !bytes.Equal(got.Secret, want.Secret) {
		t.Fatal("credential response secret bytes differ")
	}
}

func credAuditFields(t *testing.T, log *audit.Log) [][]string {
	t.Helper()
	b, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	events := make([][]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			t.Fatal("malformed audit line")
		}
		events = append(events, fields[1:])
	}
	return events
}

func assertCredAudit(t *testing.T, log *audit.Log, want ...[]string) {
	t.Helper()
	got := credAuditFields(t, log)
	if len(got) != len(want) {
		t.Fatalf("audit event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("audit event %d field count differs", i)
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("audit event %d field %d differs", i, j)
			}
		}
	}
}

func assertSecretNotRecorded(t *testing.T, deps credServeDeps, state *credTestState, secret []byte) {
	t.Helper()
	if len(secret) == 0 {
		return
	}
	b, err := os.ReadFile(deps.Audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, secret) {
		t.Fatal("secret bytes appeared in the audit log")
	}
	for _, line := range state.logLines() {
		if bytes.Contains([]byte(line), secret) {
			t.Fatal("secret bytes appeared in a warning line")
		}
	}
}

func TestServeCredRequest_Disabled(t *testing.T) {
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: []byte("unused")}}
	kc := &fakeCredKeychain{}
	deps, state := newCredTestDeps(t, p, kc)
	state.setEnabled(false)
	req := baseCredEvent()
	req.Label = "\n" + strings.Repeat("D", 250)

	resp := serveCredRequest(context.Background(), deps, req)
	assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "disabled"})
	assertCredAudit(t, deps.Audit, []string{
		"cred-denied", "host=box", "label=" + strings.Repeat("D", 200), "mode=env", "reason=disabled",
	})
	if len(p.Requests()) != 0 || len(kc.getCalls) != 0 {
		t.Fatal("disabled credential request reached prompt or Keychain")
	}
}

func TestServeCredRequest_LabelInvalid(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		auditLabel string
	}{
		{name: "control only", label: "\t\n\x00", auditLabel: ""},
		{name: "over 200 bytes", label: strings.Repeat("L", 201), auditLabel: strings.Repeat("L", 200)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowOnce}}
			kc := &fakeCredKeychain{}
			deps, _ := newCredTestDeps(t, p, kc)
			req := baseCredEvent()
			req.Label = tt.label

			resp := serveCredRequest(context.Background(), deps, req)
			assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "label-invalid"})
			assertCredAudit(t, deps.Audit, []string{
				"cred-denied", "host=box", "label=" + tt.auditLabel, "mode=env", "reason=label-invalid",
			})
			if len(p.Requests()) != 0 || len(kc.getCalls) != 0 {
				t.Fatal("invalid label reached prompt or Keychain")
			}
		})
	}
}

func TestServeCredRequest_AllowOnceAndDisplaySanitization(t *testing.T) {
	secret := []byte("allow-once-fake")
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: secret}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{{err: errors.New("locked")}}}
	deps, state := newCredTestDeps(t, p, kc)
	req := baseCredEvent()
	req.Label = "data\nbase"
	req.Requester = "pid\t42: " + strings.Repeat("R", 400)
	req.Target = strings.Repeat("P", 150) + "\n" + strings.Repeat("P", 160)

	resp := serveCredRequest(context.Background(), deps, req)
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: secret,
	})
	assertCredAudit(t, deps.Audit, []string{
		"cred-served", "host=box", "label=database", "mode=env", "source=prompt", "dur=0s",
	})
	requests := p.Requests()
	if len(requests) != 1 {
		t.Fatalf("prompt requests = %d, want 1", len(requests))
	}
	wantRequester := "pid42: " + strings.Repeat("R", 293)
	wantTarget := strings.Repeat("P", 300)
	wantPrompt := prompt.Request{
		Label: "database", Requester: wantRequester, Host: "box",
		Delivery:    `will be set as env var "` + wantTarget + `" for the requested command`,
		Remembered:  false,
		TimeoutSecs: 115,
	}
	if requests[0] != wantPrompt {
		t.Fatal("sanitized prompt request differs")
	}
	assertSecretNotRecorded(t, deps, state, secret)
}

func TestCredDeliveryText(t *testing.T) {
	tests := []struct {
		mode   string
		target string
		want   string
	}{
		{"env", "PW", `will be set as env var "PW" for the requested command`},
		{"stdin", "sudo -S", "will be piped to the command's stdin: sudo -S"},
		{"askpass", "Password:", "will be sent to sudo/askpass on the box"},
	}
	for _, tt := range tests {
		if got := credDelivery(tt.mode, tt.target); got != tt.want {
			t.Errorf("credDelivery(%q) differs", tt.mode)
		}
	}
}

func TestPromptOnlyKeychainFallsBackToFreshPrompt(t *testing.T) {
	wantErr := errors.New("home unavailable")
	kc := promptOnlyKeychain{err: wantErr}
	secret, found, err := kc.Get(context.Background(), "database")
	if err != nil || found || secret != nil {
		t.Fatal("fallback Keychain Get did not behave as not-found")
	}
	if !errors.Is(kc.Set(context.Background(), "database", []byte("fake")), wantErr) {
		t.Fatal("fallback Keychain Set did not return its construction error")
	}
	if !errors.Is(kc.Delete(context.Background(), "database"), wantErr) {
		t.Fatal("fallback Keychain Delete did not return its construction error")
	}
}

func TestServeCredRequest_AllowRememberFresh(t *testing.T) {
	secret := []byte("remember-fake")
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowRemember, Secret: secret}}
	kc := &fakeCredKeychain{}
	deps, state := newCredTestDeps(t, p, kc)

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: secret,
	})
	assertCredAudit(t, deps.Audit, []string{
		"cred-served", "host=box", "label=database", "mode=env", "source=prompt-remembered", "dur=0s",
	})
	if len(kc.sets) != 1 || kc.sets[0].label != "database" || !bytes.Equal(kc.sets[0].secret, secret) {
		t.Fatal("Keychain Set did not receive the approved label and bytes")
	}
	if len(state.logLines()) != 0 {
		t.Fatal("successful remember emitted a warning")
	}
	assertSecretNotRecorded(t, deps, state, secret)
}

func TestServeCredRequest_AllowRememberSetFailureStillServes(t *testing.T) {
	secret := []byte("remember-failure-fake")
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowRemember, Secret: secret}}
	kc := &fakeCredKeychain{setErr: errors.New("store failed")}
	deps, state := newCredTestDeps(t, p, kc)

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: secret,
	})
	assertCredAudit(t, deps.Audit, []string{
		"cred-served", "host=box", "label=database", "mode=env", "source=prompt", "dur=0s",
	})
	logs := state.logLines()
	if len(logs) != 1 || logs[0] != "cred: remember failed: store failed" {
		t.Fatal("remember failure warning differs")
	}
	assertSecretNotRecorded(t, deps, state, secret)
}

func TestServeCredRequest_RememberedAllow(t *testing.T) {
	initial := []byte("initial-fake")
	current := []byte("current-fake")
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowRemember}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{
		{secret: initial, found: true},
		{secret: current, found: true},
	}}
	deps, state := newCredTestDeps(t, p, kc)

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: current,
	})
	assertCredAudit(t, deps.Audit, []string{
		"cred-served", "host=box", "label=database", "mode=env", "source=keychain", "dur=0s",
	})
	requests := p.Requests()
	if len(requests) != 1 || !requests[0].Remembered {
		t.Fatal("remembered item did not select Dialog B")
	}
	if len(kc.getCalls) != 2 || len(kc.sets) != 0 {
		t.Fatal("remembered approval did not re-read the Keychain exactly once")
	}
	assertSecretNotRecorded(t, deps, state, current)
}

func TestServeCredRequest_RememberedItemVanishes(t *testing.T) {
	tests := []struct {
		name   string
		second fakeCredGetResult
	}{
		{name: "absent", second: fakeCredGetResult{}},
		{name: "read error", second: fakeCredGetResult{err: errors.New("locked")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowRemember}}
			kc := &fakeCredKeychain{getResults: []fakeCredGetResult{
				{secret: []byte("initial-fake"), found: true}, tt.second,
			}}
			deps, _ := newCredTestDeps(t, p, kc)

			resp := serveCredRequest(context.Background(), deps, baseCredEvent())
			assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "denied"})
			assertCredAudit(t, deps.Audit, []string{
				"cred-denied", "host=box", "label=database", "mode=env", "reason=denied",
			})
		})
	}
}

func TestServeCredRequest_RememberedForgetThenAllowOnce(t *testing.T) {
	secret := []byte("replacement-fake")
	call := 0
	p := &prompt.Fake{PromptFunc: func(context.Context, prompt.Request) (prompt.Decision, error) {
		call++
		if call == 1 {
			return prompt.Decision{Outcome: prompt.OutcomeForget}, nil
		}
		return prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: secret}, nil
	}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{{
		secret: []byte("remembered-fake"), found: true,
	}}}
	deps, state := newCredTestDeps(t, p, kc)

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: secret,
	})
	assertCredAudit(t, deps.Audit,
		[]string{"cred-forgotten", "host=box", "label=database"},
		[]string{"cred-served", "host=box", "label=database", "mode=env", "source=prompt", "dur=0s"},
	)
	requests := p.Requests()
	if len(requests) != 2 || !requests[0].Remembered || requests[1].Remembered {
		t.Fatal("Forget did not transition from Dialog B to Dialog A exactly once")
	}
	if len(kc.deletes) != 1 || kc.deletes[0] != "database" {
		t.Fatal("Forget did not delete the remembered label")
	}
	assertSecretNotRecorded(t, deps, state, secret)
}

func TestServeCredRequest_ForgetRepromptUsesRemainingDialogBudget(t *testing.T) {
	secret := []byte("budgeted-replacement-fake")
	var state *credTestState
	call := 0
	p := &prompt.Fake{PromptFunc: func(_ context.Context, req prompt.Request) (prompt.Decision, error) {
		call++
		switch call {
		case 1:
			if !req.Remembered || req.TimeoutSecs != 115 {
				t.Fatalf("first prompt = %+v, want remembered with 115 seconds", req)
			}
			state.setNow(state.nowTime().Add(100 * time.Second))
			return prompt.Decision{Outcome: prompt.OutcomeForget}, nil
		case 2:
			if req.Remembered || req.TimeoutSecs != 15 {
				t.Fatalf("replacement prompt = %+v, want fresh with 15 seconds", req)
			}
			return prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: secret}, nil
		default:
			t.Fatal("unexpected third credential prompt")
			return prompt.Decision{Outcome: prompt.OutcomeUnavailable}, nil
		}
	}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{{
		secret: []byte("remembered-fake"), found: true,
	}}}
	deps, testState := newCredTestDeps(t, p, kc)
	state = testState

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{
		Nonce: 41, Epoch: 7, OK: true, Secret: secret,
	})
	requests := p.Requests()
	if len(requests) != 2 {
		t.Fatalf("prompt requests = %d, want 2", len(requests))
	}
	if got := 100 + requests[1].TimeoutSecs; got != int(credDialogBudget/time.Second) {
		t.Fatalf("elapsed + replacement timeout = %d, want %d", got, int(credDialogBudget/time.Second))
	}
	assertCredAudit(t, deps.Audit,
		[]string{"cred-forgotten", "host=box", "label=database"},
		[]string{"cred-served", "host=box", "label=database", "mode=env", "source=prompt", "dur=1m40s"},
	)
	assertSecretNotRecorded(t, deps, state, secret)
}

func TestServeCredRequest_ForgetDoesNotRepromptBelowMinimumBudget(t *testing.T) {
	var state *credTestState
	p := &prompt.Fake{PromptFunc: func(_ context.Context, req prompt.Request) (prompt.Decision, error) {
		if req.TimeoutSecs != 115 {
			t.Fatalf("first prompt timeout = %d, want 115", req.TimeoutSecs)
		}
		state.setNow(state.nowTime().Add(112 * time.Second))
		return prompt.Decision{Outcome: prompt.OutcomeForget}, nil
	}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{{
		secret: []byte("remembered-fake"), found: true,
	}}}
	deps, testState := newCredTestDeps(t, p, kc)
	state = testState

	resp := serveCredRequest(context.Background(), deps, baseCredEvent())
	assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "timeout"})
	if len(p.Requests()) != 1 {
		t.Fatalf("prompt requests = %d, want no replacement below five seconds", len(p.Requests()))
	}
	assertCredAudit(t, deps.Audit,
		[]string{"cred-forgotten", "host=box", "label=database"},
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=timeout"},
	)
}

func TestServeCredRequest_DenyAndCooldown(t *testing.T) {
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeDeny}}
	kc := &fakeCredKeychain{getResults: []fakeCredGetResult{{
		secret: []byte("remembered-fake"), found: true,
	}}}
	deps, state := newCredTestDeps(t, p, kc)
	base := state.nowTime()
	req := baseCredEvent()
	req.Label = "data\nbase"

	resp := serveCredRequest(context.Background(), deps, req)
	assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "denied"})

	state.setNow(base.Add(9 * time.Second))
	retry := baseCredEvent()
	retry.Nonce = 42
	resp = serveCredRequest(context.Background(), deps, retry)
	assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 42, Epoch: 7, Err: "cooldown"})
	if len(p.Requests()) != 1 {
		t.Fatal("cooldown retry opened another dialog")
	}

	state.setNow(base.Add(10 * time.Second))
	retry.Nonce = 43
	resp = serveCredRequest(context.Background(), deps, retry)
	assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 43, Epoch: 7, Err: "denied"})
	requests := p.Requests()
	if len(requests) != 2 {
		t.Fatal("expired cooldown did not permit a new dialog")
	}
	if !requests[0].Remembered || requests[1].Remembered {
		t.Fatal("deny/cooldown test did not cover remembered then fresh dialogs")
	}
	assertCredAudit(t, deps.Audit,
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=denied"},
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=cooldown"},
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=denied"},
	)
	assertSecretNotRecorded(t, deps, state, []byte("remembered-fake"))
}

func TestServeCredRequest_TimeoutHasNoCooldown(t *testing.T) {
	p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeTimeout}}
	kc := &fakeCredKeychain{}
	deps, _ := newCredTestDeps(t, p, kc)

	for nonce := uint64(51); nonce <= 52; nonce++ {
		req := baseCredEvent()
		req.Nonce = nonce
		resp := serveCredRequest(context.Background(), deps, req)
		assertCredResponse(t, resp, &protocol.CredResponse{Nonce: nonce, Epoch: 7, Err: "timeout"})
	}
	if len(p.Requests()) != 2 {
		t.Fatal("timeout incorrectly activated the explicit-deny cooldown")
	}
	assertCredAudit(t, deps.Audit,
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=timeout"},
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=timeout"},
	)
}

func TestServeCredRequest_GUIUnavailable(t *testing.T) {
	tests := []struct {
		name string
		p    *prompt.Fake
	}{
		{name: "outcome", p: &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeUnavailable}}},
		{name: "prompter error", p: &prompt.Fake{Err: errors.New("no Aqua session")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _ := newCredTestDeps(t, tt.p, &fakeCredKeychain{})
			resp := serveCredRequest(context.Background(), deps, baseCredEvent())
			assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "gui-unavailable"})
			assertCredAudit(t, deps.Audit, []string{
				"cred-denied", "host=box", "label=database", "mode=env", "reason=gui-unavailable",
			})
		})
	}
}

func TestServeCredRequest_OversizeSecretDenied(t *testing.T) {
	t.Run("typed", func(t *testing.T) {
		secret := bytes.Repeat([]byte{'S'}, credSecretMaxBytes+1)
		p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: secret}}
		kc := &fakeCredKeychain{}
		deps, state := newCredTestDeps(t, p, kc)

		resp := serveCredRequest(context.Background(), deps, baseCredEvent())
		assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "denied"})
		assertCredAudit(t, deps.Audit, []string{
			"cred-denied", "host=box", "label=database", "mode=env", "reason=denied",
		})
		assertSecretNotRecorded(t, deps, state, secret)
	})

	t.Run("remembered", func(t *testing.T) {
		secret := bytes.Repeat([]byte{'K'}, credSecretMaxBytes+1)
		p := &prompt.Fake{Decision: prompt.Decision{Outcome: prompt.OutcomeAllowRemember}}
		kc := &fakeCredKeychain{getResults: []fakeCredGetResult{
			{secret: []byte("initial-fake"), found: true},
			{secret: secret, found: true},
		}}
		deps, state := newCredTestDeps(t, p, kc)

		resp := serveCredRequest(context.Background(), deps, baseCredEvent())
		assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 41, Epoch: 7, Err: "denied"})
		assertCredAudit(t, deps.Audit, []string{
			"cred-denied", "host=box", "label=database", "mode=env", "reason=denied",
		})
		assertSecretNotRecorded(t, deps, state, secret)
	})
}

func TestRunCredHandler_Busy(t *testing.T) {
	secret := []byte("handler-fake")
	started := make(chan struct{})
	release := make(chan struct{})
	p := &prompt.Fake{PromptFunc: func(ctx context.Context, _ prompt.Request) (prompt.Decision, error) {
		close(started)
		select {
		case <-release:
			return prompt.Decision{Outcome: prompt.OutcomeAllowOnce, Secret: secret}, nil
		case <-ctx.Done():
			return prompt.Decision{Outcome: prompt.OutcomeUnavailable}, ctx.Err()
		}
	}}
	deps, state := newCredTestDeps(t, p, &fakeCredKeychain{})
	events := make(chan agentclient.EngineEvent, 2)
	responses := make(chan *protocol.CredResponse, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCredHandlerWithDeps(ctx, events, deps, func(resp *protocol.CredResponse) error {
			copyResp := *resp
			copyResp.Secret = append([]byte(nil), resp.Secret...)
			responses <- &copyResp
			return nil
		})
	}()

	first := baseCredEvent()
	first.Nonce = 61
	events <- agentclient.EngineEvent{Kind: agentclient.KindCredRequest, Cred: first}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first credential prompt did not start")
	}
	second := baseCredEvent()
	second.Nonce = 62
	events <- agentclient.EngineEvent{Kind: agentclient.KindCredRequest, Cred: second}

	select {
	case resp := <-responses:
		assertCredResponse(t, resp, &protocol.CredResponse{Nonce: 62, Epoch: 7, Err: "busy"})
	case <-time.After(time.Second):
		t.Fatal("busy credential response did not arrive")
	}
	close(release)
	select {
	case resp := <-responses:
		assertCredResponse(t, resp, &protocol.CredResponse{
			Nonce: 61, Epoch: 7, OK: true, Secret: secret,
		})
	case <-time.After(time.Second):
		t.Fatal("first credential response did not arrive")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("credential handler did not stop after cancellation")
	}

	if len(p.Requests()) != 1 {
		t.Fatal("busy request reached the prompter")
	}
	assertCredAudit(t, deps.Audit,
		[]string{"cred-denied", "host=box", "label=database", "mode=env", "reason=busy"},
		[]string{"cred-served", "host=box", "label=database", "mode=env", "source=prompt", "dur=0s"},
	)
	assertSecretNotRecorded(t, deps, state, secret)
}
