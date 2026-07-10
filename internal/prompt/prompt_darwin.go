//go:build darwin

package prompt

import (
	"bytes"
	"context"
	"os/exec"
)

func newPlatformPrompter() Prompter {
	return &osascriptPrompter{run: runOSAScript}
}

func runOSAScript(ctx context.Context, script string) scriptResult {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
	}
	return scriptResult{
		stdout:   append([]byte(nil), stdout.Bytes()...),
		stderr:   append([]byte(nil), stderr.Bytes()...),
		exitCode: exitCode,
		err:      err,
	}
}
