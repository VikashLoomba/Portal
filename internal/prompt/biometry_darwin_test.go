//go:build darwin

package prompt

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBiometryAvailableOutputMapping(t *testing.T) {
	tests := []struct {
		name   string
		result scriptResult
		want   bool
	}{
		{name: "watch policy", result: scriptResult{stdout: []byte("available:4\n")}, want: true},
		{name: "biometrics policy", result: scriptResult{stdout: []byte("available:1\r\n")}, want: true},
		{name: "unavailable", result: scriptResult{stdout: []byte("unavailable\n")}},
		{name: "malformed", result: scriptResult{stdout: []byte("available:4\nunexpected")}},
		{name: "leading whitespace", result: scriptResult{stdout: []byte(" available:4\n")}},
		{name: "runner error", result: scriptResult{exitCode: -1, err: errors.New("osascript missing")}},
		{
			name: "bridge exception",
			result: scriptResult{
				stderr:   []byte("execution error: Error: exception raised by object (-2700)\n"),
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			b := &osascriptBiometry{run: func(_ context.Context, got []string) scriptResult {
				args = append([]string(nil), got...)
				return tt.result
			}}
			if got := b.Available(context.Background()); got != tt.want {
				t.Errorf("Available = %v, want %v", got, tt.want)
			}
			script := javaScriptSource(t, args)
			if script != biometryProbeScript {
				t.Fatal("availability probe did not use the fixed probe script")
			}
		})
	}
}

func TestBiometryAvailableBoundsProbe(t *testing.T) {
	var deadline time.Time
	b := &osascriptBiometry{run: func(ctx context.Context, _ []string) scriptResult {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("availability probe context has no deadline")
		}
		return scriptResult{stdout: []byte("unavailable\n")}
	}}
	before := time.Now()
	if b.Available(context.Background()) {
		t.Fatal("availability probe parsed unavailable as available")
	}
	after := time.Now()
	if deadline.Before(before.Add(biometryProbeTimeout)) ||
		deadline.After(after.Add(biometryProbeTimeout)) {
		t.Fatalf("availability probe deadline = %v, want call time + %v", deadline, biometryProbeTimeout)
	}
}

func TestBiometryProbeRunsWithoutSelectorArityError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := runOSAScript(ctx, []string{"-l", "JavaScript", "-e", biometryProbeScript})
	if strings.Contains(string(result.stderr), "wrong number of arguments") {
		t.Fatalf("availability probe raised selector arity error: %s", result.stderr)
	}
	if result.err != nil || result.exitCode != 0 {
		t.Fatalf("availability probe failed: exit=%d err=%v stderr=%s", result.exitCode, result.err, result.stderr)
	}
	switch output := string(trimResultNewline(result.stdout)); output {
	case "available:4", "available:1", "unavailable":
	default:
		t.Fatalf("availability probe output = %q", output)
	}
}

func TestBiometryApproveOutputMapping(t *testing.T) {
	tests := []struct {
		name   string
		result scriptResult
		want   BiometryOutcome
	}{
		{name: "approved token", result: scriptResult{stdout: []byte("touchid:approved\n")}, want: BiometryApproved},
		{name: "canceled token", result: scriptResult{stdout: []byte("touchid:canceled\r\n")}, want: BiometryCanceled},
		{name: "user cancel error", result: scriptResult{stdout: []byte("touchid:fallback:-2\n")}, want: BiometryCanceled},
		{name: "timeout token", result: scriptResult{stdout: []byte("touchid:timeout\n")}, want: BiometryTimeout},
		{name: "authentication failed", result: scriptResult{stdout: []byte("touchid:fallback:-1\n")}, want: BiometryFallback},
		{name: "user fallback", result: scriptResult{stdout: []byte("touchid:fallback:-3\n")}, want: BiometryFallback},
		{name: "system cancel", result: scriptResult{stdout: []byte("touchid:fallback:-4\n")}, want: BiometryFallback},
		{name: "biometry lockout", result: scriptResult{stdout: []byte("touchid:fallback:-8\n")}, want: BiometryFallback},
		{name: "app cancel", result: scriptResult{stdout: []byte("touchid:fallback:-10\n")}, want: BiometryFallback},
		{name: "invalid context", result: scriptResult{stdout: []byte("touchid:fallback:-1004\n")}, want: BiometryFallback},
		{name: "unknown LAError", result: scriptResult{stdout: []byte("touchid:fallback:-9999\n")}, want: BiometryFallback},
		{name: "caught exception token", result: scriptResult{stdout: []byte("touchid:fallback:exception\n")}, want: BiometryFallback},
		{name: "unavailable token", result: scriptResult{stdout: []byte("touchid:fallback:unavailable\n")}, want: BiometryFallback},
		{name: "malformed output", result: scriptResult{stdout: []byte("touchid:approved\nunexpected")}, want: BiometryFallback},
		{name: "leading whitespace", result: scriptResult{stdout: []byte(" touchid:approved\n")}, want: BiometryFallback},
		{name: "exception text output", result: scriptResult{stdout: []byte("Error: exception raised by object\n")}, want: BiometryFallback},
		{name: "empty output", result: scriptResult{}, want: BiometryFallback},
		{
			name: "runner error",
			result: scriptResult{
				exitCode: -1,
				err:      errors.New("osascript unavailable"),
			},
			want: BiometryFallback,
		},
		{
			name: "exception text",
			result: scriptResult{
				stderr:   []byte("execution error: Error: exception raised by object (-2700)\n"),
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			want: BiometryFallback,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &osascriptBiometry{run: func(context.Context, []string) scriptResult {
				return tt.result
			}}
			outcome, err := b.Approve(
				context.Background(),
				`portal: approve credential "database" for box`,
				time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC),
			)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != tt.want {
				t.Errorf("outcome = %d, want %d", outcome, tt.want)
			}
		})
	}
}

func TestBiometryApproveScriptContract(t *testing.T) {
	reason := `portal: approve credential "database\\admin" for box`
	deadline := time.Date(2026, 7, 28, 15, 4, 5, 678000000, time.UTC)
	var args []string
	b := &osascriptBiometry{run: func(_ context.Context, got []string) scriptResult {
		args = append([]string(nil), got...)
		return scriptResult{stdout: []byte("touchid:approved\n")}
	}}

	outcome, err := b.Approve(context.Background(), reason, deadline)
	if err != nil || outcome != BiometryApproved {
		t.Fatalf("Approve = (%d, %v), want approved", outcome, err)
	}
	script := javaScriptSource(t, args)
	for _, want := range []string{
		`ObjC.import("LocalAuthentication")`,
		`ObjC.import("Foundation")`,
		`$.LAContext.alloc.init`,
		`c.localizedCancelTitle = "Deny"`,
		`c.localizedFallbackTitle = ""`,
		`c.canEvaluatePolicyError(4, $())`,
		`c.canEvaluatePolicyError(1, $())`,
		`c.evaluatePolicyLocalizedReasonReply`,
		`function(ok, err)`,
		`$(err.code).js`,
		`$.NSRunLoop.currentRunLoop.runUntilDate`,
		`deadline.timeIntervalSinceNow`,
		`c.invalidate`,
		`try {`,
		`} catch (e) {`,
		`"touchid:approved"`,
		`"touchid:canceled"`,
		`"touchid:fallback:"`,
		`"touchid:timeout"`,
		`var reason = ` + strconv.Quote(reason),
		strconv.FormatInt(deadline.Add(-time.Second).UnixMilli(), 10) + ` / 1000`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("approval script missing %q", want)
		}
	}
	if strings.Index(script, "c.canEvaluatePolicyError(4, $())") > strings.Index(script, "c.canEvaluatePolicyError(1, $())") {
		t.Fatal("approval script did not probe policy 4 before policy 1")
	}
	for _, forbidden := range []string{
		"\nrun();",
		"c.invalidate()",
		"alloc.init()",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("approval script contains forbidden value %q", forbidden)
		}
	}
}

func TestBiometryProbeScriptContract(t *testing.T) {
	var args []string
	b := &osascriptBiometry{run: func(_ context.Context, got []string) scriptResult {
		args = append([]string(nil), got...)
		return scriptResult{stdout: []byte("available:4\n")}
	}}
	if !b.Available(context.Background()) {
		t.Fatal("availability probe did not parse available:4")
	}
	script := javaScriptSource(t, args)
	for _, want := range []string{
		`$.LAContext.alloc.init`,
		`c.canEvaluatePolicyError(4, $())`,
		`c.canEvaluatePolicyError(1, $())`,
		`"available:4"`,
		`"available:1"`,
		`"unavailable"`,
		`c.invalidate`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	if strings.Index(script, "c.canEvaluatePolicyError(4, $())") > strings.Index(script, "c.canEvaluatePolicyError(1, $())") {
		t.Fatal("probe script did not probe policy 4 before policy 1")
	}
	for _, forbidden := range []string{
		"\nrun();",
		"c.invalidate()",
		"alloc.init()",
		"evaluatePolicyLocalizedReasonReply",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("probe script contains forbidden value %q", forbidden)
		}
	}
}

func TestBiometryScriptsCannotReadCredentialSecrets(t *testing.T) {
	scripts := map[string]string{
		"approve": biometryApproveScript(
			`portal: approve credential "database" for box`,
			time.Date(2026, 7, 28, 15, 4, 5, 0, time.UTC),
		),
		"probe": biometryProbeScript,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			for _, forbidden := range []string{
				"Keychain",
				"SecItem",
				"find-generic-password",
				"portal-cred",
				"/usr/bin/security",
			} {
				if strings.Contains(script, forbidden) {
					t.Errorf("biometry script contains credential access %q", forbidden)
				}
			}
		})
	}
}

func javaScriptSource(t *testing.T, args []string) string {
	t.Helper()
	if len(args) != 4 || args[0] != "-l" || args[1] != "JavaScript" || args[2] != "-e" {
		t.Fatalf("JXA argv = %q, want [-l JavaScript -e <script>]", args)
	}
	return args[3]
}
