//go:build !darwin

package prompt

import "context"

type unavailablePrompter struct{}

func newPlatformPrompter() Prompter {
	return unavailablePrompter{}
}

// Prompt reports platform unavailability without displaying a dialog.
func (unavailablePrompter) Prompt(context.Context, Request) (Decision, error) {
	return Decision{Outcome: OutcomeUnavailable}, nil
}
