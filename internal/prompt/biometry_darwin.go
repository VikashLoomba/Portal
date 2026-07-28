//go:build darwin

package prompt

import (
	"bytes"
	"context"
	"strconv"
	"time"
)

type osascriptBiometry struct {
	run scriptRunner
}

const biometryProbeTimeout = 5 * time.Second

func newPlatformBiometry() Biometry {
	return &osascriptBiometry{run: runOSAScript}
}

// Available probes the watch-capable policy before plain biometrics.
func (b *osascriptBiometry) Available(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, biometryProbeTimeout)
	defer cancel()
	result := b.run(probeCtx, []string{"-l", "JavaScript", "-e", biometryProbeScript})
	if result.err != nil || result.exitCode != 0 {
		return false
	}
	output := trimResultNewline(result.stdout)
	return bytes.Equal(output, []byte("available:4")) ||
		bytes.Equal(output, []byte("available:1"))
}

// Approve maps every bridge or runner failure back to non-biometric consent.
func (b *osascriptBiometry) Approve(ctx context.Context, reason string, deadline time.Time) (BiometryOutcome, error) {
	result := b.run(ctx, []string{"-l", "JavaScript", "-e", biometryApproveScript(reason, deadline)})
	if result.err != nil || result.exitCode != 0 {
		return BiometryFallback, nil
	}
	return parseBiometryOutcome(result.stdout), nil
}

func parseBiometryOutcome(output []byte) BiometryOutcome {
	output = trimResultNewline(output)
	switch {
	case bytes.Equal(output, []byte("touchid:approved")):
		return BiometryApproved
	case bytes.Equal(output, []byte("touchid:canceled")):
		return BiometryCanceled
	case bytes.Equal(output, []byte("touchid:timeout")):
		return BiometryTimeout
	case bytes.Equal(output, []byte("touchid:fallback:-2")):
		return BiometryCanceled
	default:
		return BiometryFallback
	}
}

const biometryProbeScript = `ObjC.import("LocalAuthentication");

function invalidateContext(c) {
    if (c) {
        try {
            c.invalidate;
        } catch (e) {}
    }
}

function run() {
    var c;
    try {
        c = $.LAContext.alloc.init;
        var output = "unavailable";
        if (c.canEvaluatePolicyError(4, $())) {
            output = "available:4";
        } else if (c.canEvaluatePolicyError(1, $())) {
            output = "available:1";
        }
        invalidateContext(c);
        return output;
    } catch (e) {
        invalidateContext(c);
        return "unavailable";
    }
}`

func biometryApproveScript(reason string, deadline time.Time) string {
	deadlineMillis := strconv.FormatInt(deadline.Add(-time.Second).UnixMilli(), 10)
	return `ObjC.import("LocalAuthentication");
ObjC.import("Foundation");

function invalidateContext(c) {
    if (c) {
        try {
            c.invalidate;
        } catch (e) {}
    }
}

function run() {
    var c;
    try {
        c = $.LAContext.alloc.init;
        c.localizedCancelTitle = "Deny";
        c.localizedFallbackTitle = "";

        var policy = 0;
        if (c.canEvaluatePolicyError(4, $())) {
            policy = 4;
        } else if (c.canEvaluatePolicyError(1, $())) {
            policy = 1;
        } else {
            invalidateContext(c);
            return "touchid:fallback:unavailable";
        }

        var reason = ` + strconv.Quote(reason) + `;
        var deadline = $.NSDate.dateWithTimeIntervalSince1970(` + deadlineMillis + ` / 1000);
        var replied = false;
        var output = "touchid:fallback:missing-reply";
        try {
            c.evaluatePolicyLocalizedReasonReply(policy, reason, function(ok, err) {
                try {
                    if (ok) {
                        output = "touchid:approved";
                    } else {
                        var code = err ? $(err.code).js : 0;
                        output = code === -2
                            ? "touchid:canceled"
                            : "touchid:fallback:" + String(code);
                    }
                } catch (e) {
                    output = "touchid:fallback:exception";
                }
                replied = true;
            });
        } catch (e) {
            invalidateContext(c);
            return "touchid:fallback:exception";
        }

        while (!replied) {
            if (deadline.timeIntervalSinceNow <= 0) {
                invalidateContext(c);
                return "touchid:timeout";
            }
            $.NSRunLoop.currentRunLoop.runUntilDate(
                $.NSDate.dateWithTimeIntervalSinceNow(0.05)
            );
        }
        invalidateContext(c);
        return output;
    } catch (e) {
        invalidateContext(c);
        return "touchid:fallback:exception";
    }
}`
}
