//go:build !darwin

package keychain

import "context"

func defaultCommandRunner(context.Context, string, []string, []byte) commandResult {
	return commandResult{exitCode: -1, err: errSecurityUnavailable}
}
