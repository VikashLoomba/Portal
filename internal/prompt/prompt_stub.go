//go:build !darwin

package prompt

import "context"

type unavailablePrompter struct{}

func newPlatformPrompter() Prompter {
	return unavailablePrompter{}
}

func (unavailablePrompter) Prompt(context.Context, Request) (Decision, error) {
	return Decision{Outcome: OutcomeUnavailable}, nil
}
