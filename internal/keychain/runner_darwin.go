//go:build darwin

package keychain

import (
	"bytes"
	"context"
	"io"
	"os/exec"
)

func defaultCommandRunner(ctx context.Context, path string, args []string, stdin []byte) commandResult {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
	}
	return commandResult{stdout: append([]byte(nil), stdout.Bytes()...), exitCode: exitCode, err: err}
}
