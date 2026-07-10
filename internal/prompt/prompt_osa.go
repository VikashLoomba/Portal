package prompt

import (
	"bytes"
	"context"
	"strconv"

	"github.com/VikashLoomba/Portal/internal/osa"
)

type scriptResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

type scriptRunner func(context.Context, string) scriptResult

type osascriptPrompter struct {
	run scriptRunner
}

const (
	defaultDialogTimeoutSecs = 120
	minDialogTimeoutSecs     = 5
	maxDialogTimeoutSecs     = 120
)

// Prompt renders the request as the appropriate osascript dialog and maps the
// process result to a Decision without formatting or retaining secret bytes.
func (p *osascriptPrompter) Prompt(ctx context.Context, req Request) (Decision, error) {
	result := p.run(ctx, dialogScript(req))
	if result.err != nil || result.exitCode != 0 {
		if osascriptCanceled(result) {
			return Decision{Outcome: OutcomeDeny}, nil
		}
		return Decision{Outcome: OutcomeUnavailable}, nil
	}
	return parseDialogResult(result.stdout, req.Remembered), nil
}

func dialogScript(req Request) string {
	message := osa.StringLiteral(req.Label) + " & return & return & " +
		osa.StringLiteral("requested by "+req.Requester+" on "+req.Host) +
		" & return & return & " + osa.StringLiteral(req.Delivery)
	timeout := strconv.Itoa(dialogTimeoutSecs(req.TimeoutSecs))
	if req.Remembered {
		return "display dialog " + message +
			` buttons {"Deny","Forget","Allow"} default button "Allow"` +
			` cancel button "Deny" giving up after ` + timeout + ` with title "portal"`
	}
	return "display dialog " + message +
		` default answer "" buttons {"Cancel","Allow Once","Allow & Remember"}` +
		` default button "Allow Once" cancel button "Cancel" with hidden answer` +
		` giving up after ` + timeout + ` with title "portal"`
}

func dialogTimeoutSecs(requested int) int {
	if requested == 0 {
		return defaultDialogTimeoutSecs
	}
	if requested < minDialogTimeoutSecs {
		return minDialogTimeoutSecs
	}
	if requested > maxDialogTimeoutSecs {
		return maxDialogTimeoutSecs
	}
	return requested
}

func osascriptCanceled(result scriptResult) bool {
	if result.exitCode != 1 {
		return false
	}
	return bytes.Contains(result.stderr, []byte("(-128)")) ||
		bytes.Contains(result.stderr, []byte("error number -128")) ||
		bytes.Contains(result.stdout, []byte("(-128)")) ||
		bytes.Contains(result.stdout, []byte("error number -128"))
}

func parseDialogResult(output []byte, remembered bool) Decision {
	output = trimResultNewline(output)
	const gaveUpMarker = ", gave up:"
	if i := bytes.LastIndex(output, []byte(gaveUpMarker)); i >= 0 {
		gaveUp := bytes.TrimSpace(output[i+len(gaveUpMarker):])
		if bytes.Equal(gaveUp, []byte("true")) {
			return Decision{Outcome: OutcomeTimeout}
		}
		if !bytes.Equal(gaveUp, []byte("false")) {
			return Decision{Outcome: OutcomeUnavailable}
		}
		output = output[:i]
	}
	const buttonPrefix = "button returned:"
	if !bytes.HasPrefix(output, []byte(buttonPrefix)) {
		return Decision{Outcome: OutcomeUnavailable}
	}
	rest := output[len(buttonPrefix):]
	if remembered {
		switch {
		case bytes.Equal(rest, []byte("Allow")):
			return Decision{Outcome: OutcomeAllowRemember}
		case bytes.Equal(rest, []byte("Forget")):
			return Decision{Outcome: OutcomeForget}
		case bytes.Equal(rest, []byte("Deny")):
			return Decision{Outcome: OutcomeDeny}
		default:
			return Decision{Outcome: OutcomeUnavailable}
		}
	}

	const textMarker = ", text returned:"
	i := bytes.Index(rest, []byte(textMarker))
	if i < 0 {
		return Decision{Outcome: OutcomeUnavailable}
	}
	button := rest[:i]
	secret := rest[i+len(textMarker):]
	switch {
	case bytes.Equal(button, []byte("Allow Once")):
		return Decision{Outcome: OutcomeAllowOnce, Secret: append([]byte(nil), secret...)}
	case bytes.Equal(button, []byte("Allow & Remember")):
		return Decision{Outcome: OutcomeAllowRemember, Secret: append([]byte(nil), secret...)}
	case bytes.Equal(button, []byte("Cancel")):
		return Decision{Outcome: OutcomeDeny}
	default:
		return Decision{Outcome: OutcomeUnavailable}
	}
}

func trimResultNewline(output []byte) []byte {
	if len(output) > 0 && output[len(output)-1] == '\n' {
		output = output[:len(output)-1]
		if len(output) > 0 && output[len(output)-1] == '\r' {
			output = output[:len(output)-1]
		}
	}
	return output
}
