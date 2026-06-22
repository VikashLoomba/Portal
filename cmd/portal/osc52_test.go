package main

import (
	"bytes"
	"io"
	"log"
	"testing"
)

var quietLog = log.New(io.Discard, "", 0)

func TestFilterOSC52_StripBEL(t *testing.T) {
	// OSC 52 terminated by BEL, surrounded by normal output.
	in := []byte("before\x1b]52;c;aGVsbG8=\x07after")
	out, carry := filterOSC52(in, true, quietLog)
	if len(carry) != 0 {
		t.Fatalf("unexpected carry: %q", carry)
	}
	if string(out) != "beforeafter" {
		t.Errorf("got %q, want %q", out, "beforeafter")
	}
}

func TestFilterOSC52_StripST(t *testing.T) {
	// OSC 52 terminated by ST (ESC \).
	in := []byte("x\x1b]52;c;ZGF0YQ==\x1b\\y")
	out, carry := filterOSC52(in, true, quietLog)
	if len(carry) != 0 {
		t.Fatalf("unexpected carry: %q", carry)
	}
	if string(out) != "xy" {
		t.Errorf("got %q, want %q", out, "xy")
	}
}

func TestFilterOSC52_NoStripPreserves(t *testing.T) {
	in := []byte("a\x1b]52;c;Zm9v\x07b")
	out, _ := filterOSC52(in, false, quietLog)
	if !bytes.Equal(out, in) {
		t.Errorf("no-strip should preserve verbatim: got %q", out)
	}
}

func TestFilterOSC52_NoSequence(t *testing.T) {
	in := []byte("just normal text \x1b[0m with SGR but no osc52")
	out, carry := filterOSC52(in, true, quietLog)
	if len(carry) != 0 || !bytes.Equal(out, in) {
		t.Errorf("plain text altered: out=%q carry=%q", out, carry)
	}
}

func TestFilterOSC52_SplitAcrossReads(t *testing.T) {
	// Feed an OSC 52 sequence in two halves; the first call must hold the
	// incomplete sequence in carry, the second completes and strips it.
	full := []byte("pre\x1b]52;c;c3BsaXQ=\x07post")
	split := 8 // mid-sequence (inside ESC]52;c;...)
	out1, carry1 := filterOSC52(full[:split], true, quietLog)
	if string(out1) != "pre" {
		t.Errorf("first half forwarded %q, want %q", out1, "pre")
	}
	if len(carry1) == 0 {
		t.Fatal("expected carry holding the partial sequence")
	}
	out2, carry2 := filterOSC52(append(carry1, full[split:]...), true, quietLog)
	if len(carry2) != 0 {
		t.Errorf("unexpected trailing carry: %q", carry2)
	}
	if string(out2) != "post" {
		t.Errorf("second half forwarded %q, want %q", out2, "post")
	}
}

func TestFilterOSC52_MarkerSplitAcrossReads(t *testing.T) {
	// The ESC]52; marker itself is split across reads.
	full := []byte("hi\x1b]52;c;YQ==\x07")
	split := 4 // right after "hi\x1b]" — mid-marker
	out1, carry1 := filterOSC52(full[:split], true, quietLog)
	if string(out1) != "hi" {
		t.Errorf("first half: got %q, want %q", out1, "hi")
	}
	out2, carry2 := filterOSC52(append(carry1, full[split:]...), true, quietLog)
	if len(carry2) != 0 {
		t.Errorf("trailing carry: %q", carry2)
	}
	if string(out2) != "" {
		t.Errorf("second half: got %q, want empty (sequence stripped)", out2)
	}
}

func TestFilterOSC52_MultipleSequences(t *testing.T) {
	in := []byte("\x1b]52;c;AA==\x07middle\x1b]52;c;BB==\x07end")
	out, _ := filterOSC52(in, true, quietLog)
	if string(out) != "middleend" {
		t.Errorf("got %q, want %q", out, "middleend")
	}
}
