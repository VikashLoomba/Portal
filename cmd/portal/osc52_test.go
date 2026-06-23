package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
)

var quietLog = log.New(io.Discard, "", 0)

// chunkReader yields the data one fixed-size chunk per Read, so a test can
// exercise copyFilteringOSC52's cross-read state machine the way a real PTY
// (which delivers output in arbitrary fragments) would.
type chunkReader struct {
	data []byte
	n    int // bytes per Read
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.n
	if end > len(c.data) {
		end = len(c.data)
	}
	k := copy(p, c.data[c.pos:end])
	c.pos += k
	return k, nil
}

// cappedWriter records everything written and enforces a hard ceiling on the
// total bytes accepted, so a runaway/never-draining filter fails loudly with a
// bounded test instead of OOMing the whole suite.
type cappedWriter struct {
	buf bytes.Buffer
	cap int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.cap {
		// Accept up to the cap so the test can assert on what made it out.
		room := w.cap - w.buf.Len()
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

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

// F2 + F9: an unterminated «ESC]52;...» marker must NOT grow the carry without
// bound nor freeze the display. We drive the real streaming path
// (copyFilteringOSC52) with a reader that emits a marker followed by megabytes
// of payload that never terminates, then keeps emitting plain output. The
// filter must (a) bound what it buffers and (b) eventually let output through.
func TestCopyFilteringOSC52_UnterminatedIsBounded(t *testing.T) {
	// One marker, then far more "payload" than the cap, never terminated,
	// then a clearly-identifiable trailer that MUST reach the screen.
	var b bytes.Buffer
	b.WriteString("start")
	b.Write(osc52Prefix)
	b.WriteString("c;")                               // valid framing so we enter oscBody
	b.WriteString(strings.Repeat("A", osc52MaxSeq*4)) // base64-ish, never terminated
	b.WriteString("TAILTAIL")                         // must not be withheld forever
	in := b.Bytes()

	src := &chunkReader{data: in, n: 1024}
	dst := &cappedWriter{cap: len(in) + 4096} // generous, but bounded
	copyFilteringOSC52(dst, src, true, quietLog)

	out := dst.buf.String()
	if !strings.HasPrefix(out, "start") {
		t.Errorf("leading output dropped: %.16q", out)
	}
	// The display must not be frozen: the trailer following the overflowed,
	// never-terminated marker has to make it through.
	if !strings.Contains(out, "TAILTAIL") {
		t.Fatalf("output frozen: trailer never reached the screen (len(out)=%d)", len(out))
	}
}

// Regression: the frame-state overflow path must advance past the buffered
// byte BEFORE resyncing, exactly like body. Earlier code appended the byte,
// checked the cap, then resynced at the same index — duplicating that byte.
// We feed the marker + valid selection prefix followed by a long run of
// selection bytes with no ';' (which keeps the parser in oscFrame until it
// overflows). Since this is never a real OSC 52 write, strip mode forwards it
// verbatim, so the output must equal the input byte-for-byte — one extra byte
// would mean the overflow byte was emitted twice.
func TestCopyFilteringOSC52_FrameOverflowNoDuplicate(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("start")
	b.Write(osc52Prefix)
	b.WriteString(strings.Repeat("c", osc52MaxSeq*2)) // selection bytes, never a ';'
	b.WriteString("TAILTAIL")
	in := b.Bytes()

	src := &chunkReader{data: in, n: 1024}
	dst := &cappedWriter{cap: len(in) + 4096}
	copyFilteringOSC52(dst, src, true, quietLog)

	out := dst.buf.Bytes()
	if !bytes.Equal(out, in) {
		t.Errorf("frame-overflow output diverged from input (len out=%d, in=%d) — likely a duplicated byte", len(out), len(in))
	}
}

// Stronger F2 guard: assert the parser's in-progress buffer itself never
// exceeds the cap, regardless of how the bytes are chunked.
func TestOSC52Filter_SeqBufferCapped(t *testing.T) {
	f := &osc52Filter{strip: true, dbg: quietLog}
	var out []byte
	// Feed the marker + valid framing, then a long un-terminated run in
	// many small chunks.
	feed := func(s string) { out = f.feed(out[:0], []byte(s)) }
	feed(string(osc52Prefix))
	feed("c;")
	for i := 0; i < osc52MaxSeq*4; i++ {
		feed("A")
		if len(f.seq) > osc52MaxSeq+2 { // +2 slack for the final appended byte
			t.Fatalf("seq buffer grew past cap: %d > %d", len(f.seq), osc52MaxSeq)
		}
	}
}

// F7: a string that merely looks like the marker but is embedded inside an
// OSC 0/2 window-title sequence must NOT be mis-stripped, and the title's own
// terminator must not be swallowed. Here "\x1b]52;" appears inside a title but
// the bytes after it do not form a valid OSC 52 payload, so framing fails and
// the whole title is preserved verbatim.
func TestFilterOSC52_EmbeddedInTitleNotStripped(t *testing.T) {
	// Window-title OSC 2 whose text happens to contain the 5-byte marker
	// followed by a space (not a valid selection/data shape).
	in := []byte("\x1b]2;build \x1b]52; mode\x07done")
	out, carry := filterOSC52(in, true, quietLog)
	if len(carry) != 0 {
		t.Fatalf("unexpected carry: %q", carry)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("title with embedded marker altered:\n got  %q\n want %q", out, in)
	}
}

// F7: while scanning for an OSC 52 terminator, a fresh «ESC]» opener means the
// marker was not a real clipboard write — we must abort and forward the bytes
// rather than swallow the unrelated OSC that follows. The trailing legitimate
// title and its terminator must survive intact.
func TestFilterOSC52_FreshOpenerAbortsScan(t *testing.T) {
	// "\x1b]52;c;AA" looks like the start of a real write, but then a NEW
	// OSC opener appears before any terminator. The candidate is abandoned.
	in := []byte("x\x1b]52;c;AA\x1b]2;title\x07y")
	out, carry := filterOSC52(in, true, quietLog)
	if len(carry) != 0 {
		t.Fatalf("unexpected carry: %q", carry)
	}
	// Everything must be preserved: the bogus candidate is flushed verbatim,
	// and the genuine title OSC that follows is left alone.
	if !bytes.Equal(out, in) {
		t.Errorf("fresh-opener abort altered output:\n got  %q\n want %q", out, in)
	}
}

// EOF flush (LOW): when the stream ends with a dangling, unterminated OSC 52
// marker, the strip guarantee must hold — the raw marker must NOT be emitted.
func TestCopyFilteringOSC52_EOFDanglingMarkerDropped(t *testing.T) {
	in := []byte("hello\x1b]52;c;Zm9v") // valid framing, no terminator, then EOF
	src := &chunkReader{data: in, n: 64}
	var dst bytes.Buffer
	copyFilteringOSC52(&dst, src, true, quietLog)

	out := dst.String()
	if out != "hello" {
		t.Errorf("EOF flush emitted dangling marker: got %q, want %q", out, "hello")
	}
	if bytes.Contains([]byte(out), osc52Prefix) {
		t.Errorf("raw OSC52 marker leaked to output: %q", out)
	}
}

// EOF flush in pass-through (no-strip) mode must NOT lose bytes: when we are
// not stripping, our only contract is to leave the stream unaltered, so a
// dangling marker is forwarded verbatim at EOF.
func TestCopyFilteringOSC52_EOFDanglingMarkerForwardedWhenNotStripping(t *testing.T) {
	in := []byte("hello\x1b]52;c;Zm9v")
	src := &chunkReader{data: in, n: 64}
	var dst bytes.Buffer
	copyFilteringOSC52(&dst, src, false, quietLog)

	if dst.String() != string(in) {
		t.Errorf("no-strip EOF altered stream: got %q, want %q", dst.String(), in)
	}
}

// Sanity: the streaming path strips a complete sequence delivered across many
// tiny reads, proving the cross-read state machine matches the one-shot helper.
func TestCopyFilteringOSC52_StripsAcrossTinyReads(t *testing.T) {
	in := []byte("A\x1b]52;c;aGVsbG8=\x07B\x1b]52;c;d29ybGQ=\x1b\\C")
	for _, chunk := range []int{1, 2, 3, 5, 7, 64} {
		src := &chunkReader{data: in, n: chunk}
		var dst bytes.Buffer
		copyFilteringOSC52(&dst, src, true, quietLog)
		if dst.String() != "ABC" {
			t.Errorf("chunk=%d: got %q, want %q", chunk, dst.String(), "ABC")
		}
	}
}
