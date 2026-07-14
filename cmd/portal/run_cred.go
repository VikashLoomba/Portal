package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/keychain"
	"github.com/VikashLoomba/Portal/internal/prompt"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

const (
	credLabelMaxBytes   = 200
	credContextMaxBytes = 300
	credSecretMaxBytes  = 4096
	credDenyCooldown    = 10 * time.Second
	credDialogBudget    = 115 * time.Second
	credDialogMinSecs   = 5
	credDialogMaxSecs   = 120
	// C1 has no "oversize" wire reason; an oversized secret fails closed under
	// the generic denied token rather than inventing a new protocol value.
	credInvalidSecretReason = "denied"
)

var errCredKeychainUnavailable = errors.New("credential keychain unavailable")

// credKeychain is the remembered-credential subset used by the serve path.
// internal/keychain.Store satisfies it; tests provide a hermetic fake.
type credKeychain interface {
	// Get reads a remembered secret and reports whether the item exists.
	Get(ctx context.Context, label string) ([]byte, bool, error)
	// Set persists a newly approved secret under label.
	Set(ctx context.Context, label string, secret []byte) error
	// Delete removes a remembered item, tolerating absence in production.
	Delete(ctx context.Context, label string) error
}

var (
	_ credKeychain = (*keychain.Store)(nil)
	_ credKeychain = promptOnlyKeychain{}
)

// credCooldown tracks the last explicit denial per sanitized credential label.
// It is safe for concurrent workers even though the handler normally serializes
// prompts with its semaphore.
type credCooldown struct {
	mu     sync.Mutex
	denied map[string]time.Time
}

func newCredCooldown() *credCooldown {
	return &credCooldown{denied: make(map[string]time.Time)}
}

func (c *credCooldown) active(label string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.denied[label]
	if !ok {
		return false
	}
	if now.Before(at.Add(credDenyCooldown)) {
		return true
	}
	delete(c.denied, label)
	return false
}

func (c *credCooldown) record(label string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.denied[label] = now
	c.mu.Unlock()
}

// credServeDeps is the fully injectable boundary for one credential request.
// Log receives only fixed context and dependency errors, never secret bytes.
type credServeDeps struct {
	Prompter       prompt.Prompter
	KC             credKeychain
	FeatureEnabled func(string) bool
	Audit          *audit.Log
	Host           string
	Cooldown       *credCooldown
	Now            func() time.Time
	Log            func(string)
}

type credResponseSender func(*protocol.CredResponse) error

// promptOnlyKeychain is the keychain.New failure fallback. Reads behave as
// not-found so Dialog A remains available; writes return the construction error
// so Allow & Remember degrades to a one-time approval without losing delivery.
type promptOnlyKeychain struct {
	err error
}

// Get degrades every lookup to not-found so a fresh prompt remains available.
func (promptOnlyKeychain) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

// Set returns the construction error so remembering degrades to allow-once.
func (p promptOnlyKeychain) Set(context.Context, string, []byte) error {
	return p.err
}

// Delete returns the construction error because no backing store exists.
func (p promptOnlyKeychain) Delete(context.Context, string) error {
	return p.err
}

// runCredHandler drains the dedicated credential channel and delegates to the
// injectable handler loop. Production constructs one prompter, Keychain Store,
// and cooldown map for the lifetime of this daemon run.
func runCredHandler(ctx context.Context, ch <-chan agentclient.EngineEvent, a *app.App, wg *sync.WaitGroup) {
	logLine := func(line string) {
		if a.Log != nil {
			a.Log.Logf("%s", line)
		}
	}
	kc, err := keychain.New()
	var store credKeychain = kc
	if err != nil {
		logLine("cred: keychain unavailable: " + err.Error())
		store = promptOnlyKeychain{err: err}
	}
	deps := credServeDeps{
		Prompter:       prompt.New(),
		KC:             store,
		FeatureEnabled: a.Cfg.FeatureEnabled,
		Audit:          a.Audit,
		Host:           a.Transport.Describe().Host,
		Cooldown:       newCredCooldown(),
		Now:            time.Now,
		Log:            logLine,
	}
	runCredHandlerWithDeps(ctx, ch, deps, a.AgentClient.SendCredResponse, wg)
}

func runCredHandlerWithDeps(ctx context.Context, ch <-chan agentclient.EngineEvent,
	deps credServeDeps, send credResponseSender, wg *sync.WaitGroup) {

	sem := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Cred == nil {
				continue
			}
			req := ev.Cred
			select {
			case sem <- struct{}{}:
			default:
				label := truncateCredText(stripCredControls(req.Label), credLabelMaxBytes)
				deps.Audit.CredDenied(deps.Host, label, req.Mode, "busy")
				resp := &protocol.CredResponse{
					Nonce: req.Nonce, Epoch: req.Epoch, OK: false, Err: "busy",
				}
				if err := send(resp); err != nil {
					logCredLine(deps, fmt.Sprintf("cred: send response failed (nonce=%d): %v", req.Nonce, err))
				}
				continue
			}
			if wg != nil {
				wg.Add(1)
			}
			go func(req *agentclient.CredEvent) {
				if wg != nil {
					defer wg.Done()
				}
				defer func() { <-sem }()
				resp := serveCredRequest(ctx, deps, req)
				if err := send(resp); err != nil {
					logCredLine(deps, fmt.Sprintf("cred: send response failed (nonce=%d): %v", req.Nonce, err))
				}
			}(req)
		}
	}
}

// serveCredRequest applies the credential feature gate, display sanitization,
// cooldown, Keychain confirmation, prompt decision, and audit mapping. It NEVER
// returns nil and echoes the request Nonce/Epoch on every path.
func serveCredRequest(ctx context.Context, deps credServeDeps, req *agentclient.CredEvent) *protocol.CredResponse {
	if req == nil {
		req = &agentclient.CredEvent{}
	}
	resp := &protocol.CredResponse{Nonce: req.Nonce, Epoch: req.Epoch}
	label := stripCredControls(req.Label)
	auditLabel := truncateCredText(label, credLabelMaxBytes)

	if deps.FeatureEnabled == nil || !deps.FeatureEnabled(config.FeatureCred) {
		return denyCredResponse(deps, resp, auditLabel, req.Mode, "disabled")
	}

	if label == "" || len(label) > credLabelMaxBytes {
		return denyCredResponse(deps, resp, auditLabel, req.Mode, "label-invalid")
	}
	now := credNow(deps)
	if deps.Cooldown.active(label, now) {
		return denyCredResponse(deps, resp, label, req.Mode, "cooldown")
	}

	requester := truncateCredText(stripCredControls(req.Requester), credContextMaxBytes)
	target := truncateCredText(stripCredControls(req.Target), credContextMaxBytes)
	promptReq := prompt.Request{
		Label: label, Requester: requester, Host: deps.Host,
		Delivery: credDelivery(req.Mode, target),
	}
	started := now
	deadline := started.Add(credDialogBudget)
	remembered := false
	if deps.KC != nil {
		_, found, err := deps.KC.Get(ctx, label)
		remembered = err == nil && found
	}
	promptReq.Remembered = remembered
	timeoutSecs, ok := credPromptTimeoutSecs(deps, deadline)
	if !ok {
		return denyCredResponse(deps, resp, label, req.Mode, "timeout")
	}
	promptReq.TimeoutSecs = timeoutSecs
	decision := credPrompt(ctx, deps.Prompter, promptReq)

	if !remembered {
		return resolveFreshCredDecision(ctx, deps, req.Mode, label, started, resp, decision)
	}

	switch decision.Outcome {
	case prompt.OutcomeAllowRemember:
		secret, found, err := deps.KC.Get(ctx, label)
		if err != nil || !found || len(secret) > credSecretMaxBytes {
			return denyCredResponse(deps, resp, label, req.Mode, credInvalidSecretReason)
		}
		return serveCredResponse(deps, resp, label, req.Mode, "keychain", started, secret)
	case prompt.OutcomeForget:
		_ = deps.KC.Delete(ctx, label)
		deps.Audit.CredForgotten(deps.Host, label)
		promptReq.Remembered = false
		timeoutSecs, ok = credPromptTimeoutSecs(deps, deadline)
		if !ok {
			return denyCredResponse(deps, resp, label, req.Mode, "timeout")
		}
		promptReq.TimeoutSecs = timeoutSecs
		decision = credPrompt(ctx, deps.Prompter, promptReq)
		return resolveFreshCredDecision(ctx, deps, req.Mode, label, started, resp, decision)
	case prompt.OutcomeDeny:
		deps.Cooldown.record(label, credNow(deps))
		return denyCredResponse(deps, resp, label, req.Mode, "denied")
	case prompt.OutcomeTimeout:
		return denyCredResponse(deps, resp, label, req.Mode, "timeout")
	case prompt.OutcomeUnavailable:
		return denyCredResponse(deps, resp, label, req.Mode, "gui-unavailable")
	default:
		return denyCredResponse(deps, resp, label, req.Mode, "gui-unavailable")
	}
}

func resolveFreshCredDecision(ctx context.Context, deps credServeDeps, mode, label string,
	started time.Time, resp *protocol.CredResponse, decision prompt.Decision) *protocol.CredResponse {

	switch decision.Outcome {
	case prompt.OutcomeAllowOnce:
		if len(decision.Secret) > credSecretMaxBytes {
			return denyCredResponse(deps, resp, label, mode, credInvalidSecretReason)
		}
		return serveCredResponse(deps, resp, label, mode, "prompt", started, decision.Secret)
	case prompt.OutcomeAllowRemember:
		if len(decision.Secret) > credSecretMaxBytes {
			return denyCredResponse(deps, resp, label, mode, credInvalidSecretReason)
		}
		err := errCredKeychainUnavailable
		if deps.KC != nil {
			err = deps.KC.Set(ctx, label, decision.Secret)
		}
		source := "prompt-remembered"
		if err != nil {
			source = "prompt"
			logCredLine(deps, "cred: remember failed: "+err.Error())
		}
		return serveCredResponse(deps, resp, label, mode, source, started, decision.Secret)
	case prompt.OutcomeDeny:
		deps.Cooldown.record(label, credNow(deps))
		return denyCredResponse(deps, resp, label, mode, "denied")
	case prompt.OutcomeTimeout:
		return denyCredResponse(deps, resp, label, mode, "timeout")
	case prompt.OutcomeUnavailable:
		return denyCredResponse(deps, resp, label, mode, "gui-unavailable")
	default:
		return denyCredResponse(deps, resp, label, mode, "gui-unavailable")
	}
}

func credPrompt(ctx context.Context, prompter prompt.Prompter, req prompt.Request) prompt.Decision {
	if prompter == nil {
		return prompt.Decision{Outcome: prompt.OutcomeUnavailable}
	}
	decision, err := prompter.Prompt(ctx, req)
	if err != nil {
		return prompt.Decision{Outcome: prompt.OutcomeUnavailable}
	}
	return decision
}

func serveCredResponse(deps credServeDeps, resp *protocol.CredResponse, label, mode, source string,
	started time.Time, secret []byte) *protocol.CredResponse {

	deps.Audit.CredServed(deps.Host, label, mode, source, credDuration(deps, started))
	resp.OK = true
	resp.Secret = append([]byte(nil), secret...)
	return resp
}

func denyCredResponse(deps credServeDeps, resp *protocol.CredResponse, label, mode, reason string) *protocol.CredResponse {
	deps.Audit.CredDenied(deps.Host, label, mode, reason)
	resp.OK = false
	resp.Secret = nil
	resp.Err = reason
	return resp
}

func credDuration(deps credServeDeps, started time.Time) time.Duration {
	dur := credNow(deps).Sub(started)
	if dur < 0 {
		return 0
	}
	return dur
}

func credNow(deps credServeDeps) time.Time {
	if deps.Now == nil {
		return time.Now()
	}
	return deps.Now()
}

func credPromptTimeoutSecs(deps credServeDeps, deadline time.Time) (int, bool) {
	remaining := deadline.Sub(credNow(deps))
	seconds := int(remaining / time.Second)
	if seconds < credDialogMinSecs {
		return 0, false
	}
	if seconds > credDialogMaxSecs {
		return credDialogMaxSecs, true
	}
	return seconds, true
}

func logCredLine(deps credServeDeps, line string) {
	if deps.Log != nil {
		deps.Log(line)
	}
}

func credDelivery(mode, target string) string {
	switch mode {
	case "env":
		return `will be set as env var "` + target + `" for the requested command`
	case "stdin":
		return "will be piped to the command's stdin: " + target
	case "askpass":
		return "will be sent to sudo/askpass on the box"
	default:
		return ""
	}
}

func stripCredControls(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncateCredText(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	var b strings.Builder
	b.Grow(maxBytes)
	for _, r := range value {
		n := utf8.RuneLen(r)
		if n < 0 || b.Len()+n > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
