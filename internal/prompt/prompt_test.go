package prompt

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/osa"
)

func TestDialogACommandConstruction(t *testing.T) {
	label := `db "root"\admin`
	requester := `pid 42: sh -c "deploy"`
	delivery := `will be set as env var PW for: sh -c "curl $PW"`
	var script string
	p := &osascriptPrompter{run: func(_ context.Context, args []string) scriptResult {
		script = appleScriptSource(t, args)
		return scriptResult{stdout: []byte("button returned:Allow Once, text returned:s3kr3t, gave up:false\n")}
	}}
	decision, err := p.Prompt(context.Background(), Request{
		Label: label, Requester: requester, Host: "box", Delivery: delivery, TimeoutSecs: 45,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != OutcomeAllowOnce || !bytes.Equal(decision.Secret, []byte("s3kr3t")) {
		t.Fatalf("outcome = %d, secret length = %d; want allow-once with 7 bytes", decision.Outcome, len(decision.Secret))
	}
	for _, want := range []string{
		`with hidden answer`,
		`default answer ""`,
		`buttons {"Cancel","Allow Once","Allow & Remember"}`,
		`default button "Allow Once"`,
		`cancel button "Cancel"`,
		`giving up after 45`,
		`with title "portal"`,
		osa.StringLiteral(label),
		osa.StringLiteral("requested by " + requester + " on box"),
		osa.StringLiteral(delivery),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Dialog A script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, label) {
		t.Fatalf("attacker-influenced label appeared raw in script:\n%s", script)
	}
	if strings.Contains(script, "Remember stores this in your Mac Keychain; future approvals for this credential use Touch ID.") {
		t.Fatalf("Dialog A script contains Touch ID enrollment message:\n%s", script)
	}
}

func TestDialogAEnrollmentCommandConstruction(t *testing.T) {
	script := dialogScript(Request{
		Label: "sudo", Requester: "pid 9: sudo", Host: "box",
		Delivery: "will be sent to sudo/askpass on the box", TouchIDEnroll: true,
	})
	for _, want := range []string{
		`buttons {"Cancel","Allow Once","Allow & Remember"}`,
		`default button "Allow & Remember"`,
		`cancel button "Cancel"`,
		osa.StringLiteral("Remember stores this in your Mac Keychain; future approvals for this credential use Touch ID."),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Dialog A enrollment script missing %q:\n%s", want, script)
		}
	}
}

func TestDialogTimeoutClamp(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{name: "default", requested: 0, want: 120},
		{name: "minimum", requested: 1, want: 5},
		{name: "within bounds", requested: 115, want: 115},
		{name: "maximum", requested: 121, want: 120},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := dialogScript(Request{TimeoutSecs: tt.requested})
			want := "giving up after " + strconv.Itoa(tt.want)
			if !strings.Contains(script, want) {
				t.Fatalf("dialog script missing %q: %s", want, script)
			}
		})
	}
}

func TestDialogBCommandConstruction(t *testing.T) {
	var script string
	p := &osascriptPrompter{run: func(_ context.Context, args []string) scriptResult {
		script = appleScriptSource(t, args)
		return scriptResult{stdout: []byte("button returned:Allow, gave up:false\n")}
	}}
	decision, err := p.Prompt(context.Background(), Request{
		Label: "sudo", Requester: "pid 9: sudo", Host: "box",
		Delivery: "will be piped to sudo", Remembered: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != OutcomeAllowRemember || len(decision.Secret) != 0 {
		t.Fatalf("outcome = %d, secret length = %d; want remembered approval without typed secret", decision.Outcome, len(decision.Secret))
	}
	for _, want := range []string{
		`buttons {"Deny","Forget","Allow"}`,
		`default button "Allow"`,
		`cancel button "Deny"`,
		`giving up after 120`,
		`with title "portal"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Dialog B script missing %q:\n%s", want, script)
		}
	}
	for _, forbidden := range []string{
		"hidden answer",
		"default answer",
		"Remember stores this in your Mac Keychain; future approvals for this credential use Touch ID.",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("Dialog B script contains %q:\n%s", forbidden, script)
		}
	}
}

func TestDialogOutputMapping(t *testing.T) {
	tests := []struct {
		name       string
		remembered bool
		result     scriptResult
		want       Outcome
		secret     []byte
	}{
		{
			name:   "allow once",
			result: scriptResult{stdout: []byte("button returned:Allow Once, text returned:once, gave up:false\n")},
			want:   OutcomeAllowOnce,
			secret: []byte("once"),
		},
		{
			name:   "allow and remember",
			result: scriptResult{stdout: []byte("button returned:Allow & Remember, text returned:stored\n")},
			want:   OutcomeAllowRemember,
			secret: []byte("stored"),
		},
		{
			name:   "normal cancel result",
			result: scriptResult{stdout: []byte("button returned:Cancel, text returned:ignored")},
			want:   OutcomeDeny,
		},
		{
			name:   "new dialog timeout real output",
			result: scriptResult{stdout: []byte("button returned:, text returned:, gave up:true\n")},
			want:   OutcomeTimeout,
		},
		{
			name:       "remembered allow",
			remembered: true,
			result:     scriptResult{stdout: []byte("button returned:Allow, gave up:false\n")},
			want:       OutcomeAllowRemember,
		},
		{
			name:       "remembered forget",
			remembered: true,
			result:     scriptResult{stdout: []byte("button returned:Forget, gave up:false\n")},
			want:       OutcomeForget,
		},
		{
			name:       "remembered deny",
			remembered: true,
			result:     scriptResult{stdout: []byte("button returned:Deny, gave up:false\n")},
			want:       OutcomeDeny,
		},
		{
			name:       "remembered timeout real output",
			remembered: true,
			result:     scriptResult{stdout: []byte("button returned:, gave up:true\n")},
			want:       OutcomeTimeout,
		},
		{
			name:   "cancel exit minus 128",
			result: scriptResult{stderr: []byte("execution error: User canceled. (-128)\n"), exitCode: 1, err: errors.New("exit status 1")},
			want:   OutcomeDeny,
		},
		{
			name:   "other osascript exit",
			result: scriptResult{stderr: []byte("execution error: no GUI (-600)\n"), exitCode: 1, err: errors.New("exit status 1")},
			want:   OutcomeUnavailable,
		},
		{
			name:   "runner unavailable",
			result: scriptResult{exitCode: -1, err: errors.New("executable unavailable")},
			want:   OutcomeUnavailable,
		},
		{
			name:   "malformed output",
			result: scriptResult{stdout: []byte("unexpected")},
			want:   OutcomeUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &osascriptPrompter{run: func(context.Context, []string) scriptResult {
				return tt.result
			}}
			decision, err := p.Prompt(context.Background(), Request{Remembered: tt.remembered})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Outcome != tt.want {
				t.Errorf("outcome = %d, want %d", decision.Outcome, tt.want)
			}
			if !bytes.Equal(decision.Secret, tt.secret) {
				t.Errorf("secret bytes differ")
			}
		})
	}
}

func appleScriptSource(t *testing.T, args []string) string {
	t.Helper()
	if len(args) != 2 || args[0] != "-e" {
		t.Fatalf("AppleScript argv = %q, want [-e <script>]", args)
	}
	return args[1]
}

func TestFakeRecordsRequestsAndCopiesSecret(t *testing.T) {
	f := &Fake{Decision: Decision{Outcome: OutcomeAllowOnce, Secret: []byte("fake-secret")}}
	request := Request{Label: "database", Host: "box"}
	decision, err := f.Prompt(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	decision.Secret[0] = 'X'
	requests := f.Requests()
	if len(requests) != 1 || requests[0] != request {
		t.Fatalf("requests = %#v, want %#v", requests, []Request{request})
	}
	if !bytes.Equal(f.Decision.Secret, []byte("fake-secret")) {
		t.Fatal("Fake returned its configured secret slice without copying it")
	}
}

func TestNonDarwinPrompterIsUnavailable(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin uses the osascript implementation")
	}
	decision, err := New().Prompt(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != OutcomeUnavailable {
		t.Errorf("outcome = %d, want unavailable", decision.Outcome)
	}
}
