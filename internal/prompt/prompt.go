// Package prompt presents credential consent dialogs without exposing secrets
// to logs, formatting helpers, or callers other than the returned Decision.
package prompt

import (
	"context"
	"sync"
)

// Outcome is the user's decision, a dialog timeout, or platform unavailability.
type Outcome uint8

const (
	// OutcomeAllowOnce approves this request without persisting the secret.
	OutcomeAllowOnce Outcome = iota + 1
	// OutcomeAllowRemember approves this request and asks the caller to use or
	// persist the returned secret. A remembered-item confirmation returns this
	// outcome with an empty Secret so the caller can read the Keychain item.
	OutcomeAllowRemember
	// OutcomeDeny records an explicit user denial or cancellation.
	OutcomeDeny
	// OutcomeForget asks the caller to delete a remembered item and prompt again.
	OutcomeForget
	// OutcomeTimeout records that the consent dialog gave up without a decision.
	OutcomeTimeout
	// OutcomeUnavailable records that no usable GUI/Aqua prompt is available.
	OutcomeUnavailable
)

// Request is the display-only context shown in a credential consent dialog.
// Delivery is caller-composed human text explaining where the secret will go;
// Remembered selects the confirmation-only dialog for an existing item.
type Request struct {
	// Label identifies the credential requested by the box.
	Label string
	// Requester identifies the remote process asking for the credential.
	Requester string
	// Host identifies the remote box to the user.
	Host string
	// Delivery explains how the approved secret will reach its consumer.
	Delivery string
	// Remembered selects the confirmation dialog with no secret text field.
	Remembered bool
	// TimeoutSecs is the dialog's auto-dismiss interval. Zero selects the
	// production default; the platform implementation clamps non-zero values.
	TimeoutSecs int
}

// Decision contains the prompt outcome and, only for a newly typed approval,
// the secret bytes. It deliberately has no String method.
type Decision struct {
	// Outcome is the user's choice or the prompt failure mode.
	Outcome Outcome
	// Secret is populated only by an approved secure-input dialog.
	Secret []byte
}

// Prompter presents one credential consent request.
type Prompter interface {
	Prompt(ctx context.Context, req Request) (Decision, error)
}

// New returns the platform credential prompter. Non-macOS platforms return a
// prompter whose decisions are always OutcomeUnavailable.
func New() Prompter {
	return newPlatformPrompter()
}

// Fake is a concurrency-safe Prompter for handler tests. PromptFunc, when set,
// takes precedence over Decision and Err; every request is retained for later
// inspection through Requests.
type Fake struct {
	// PromptFunc optionally computes a decision for each request.
	PromptFunc func(context.Context, Request) (Decision, error)
	// Decision is returned when PromptFunc is nil.
	Decision Decision
	// Err is returned when PromptFunc is nil.
	Err error

	mu       sync.Mutex
	requests []Request
}

// Prompt records req and returns the configured fake result.
func (f *Fake) Prompt(ctx context.Context, req Request) (Decision, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	fn := f.PromptFunc
	decision := cloneDecision(f.Decision)
	err := f.Err
	f.mu.Unlock()
	if fn != nil {
		decision, err = fn(ctx, req)
		return cloneDecision(decision), err
	}
	return decision, err
}

// Requests returns a snapshot of all requests seen by the fake.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

func cloneDecision(decision Decision) Decision {
	decision.Secret = append([]byte(nil), decision.Secret...)
	return decision
}
