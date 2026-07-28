package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/clipupload"
)

type fakeCopySocket struct {
	path        string
	requests    chan string
	inspections chan error
	stop        func()
}

func startFakeCopySocket(t *testing.T, dir, name, reply string) fakeCopySocket {
	t.Helper()
	return startFakeCopySocketWithInspect(t, dir, name, reply, nil)
}

func startFakeCopySocketWithInspect(
	t *testing.T,
	dir, name, reply string,
	inspect func(string) error,
) fakeCopySocket {
	t.Helper()
	path := filepath.Join(dir, name)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	requests := make(chan string, 8)
	inspections := make(chan error, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if line != "" {
				requests <- line
				if inspect != nil {
					inspections <- inspect(line)
				}
				_, _ = io.WriteString(conn, reply)
			}
			_ = conn.Close()
		}
	}()
	return fakeCopySocket{
		path:        path,
		requests:    requests,
		inspections: inspections,
		stop: func() {
			_ = listener.Close()
			<-done
		},
	}
}

func startStalledCopySocket(t *testing.T, dir, name string) fakeCopySocket {
	t.Helper()
	path := filepath.Join(dir, name)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	requests := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(conn).ReadString('\n')
		if line != "" {
			requests <- line
		}
		<-release
		_ = conn.Close()
	}()
	return fakeCopySocket{
		path:     path,
		requests: requests,
		stop: func() {
			_ = listener.Close()
			close(release)
			<-done
		},
	}
}

func shortCopySocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func runClipCopyBin(
	t *testing.T,
	bin, home string,
	stdin []byte,
	args ...string,
) (stdout, stderr []byte, code int) {
	t.Helper()
	full := append([]string{"clip", "copy"}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		if cmd.ProcessState == nil {
			t.Fatalf("run clip copy: %v", err)
		}
		code = cmd.ProcessState.ExitCode()
	}
	return out.Bytes(), errOut.Bytes(), code
}

func waitCopyRequest(t *testing.T, fake fakeCopySocket) string {
	t.Helper()
	select {
	case line := <-fake.requests:
		return line
	case <-time.After(time.Second):
		t.Fatal("fake agent did not receive copy request")
		return ""
	}
}

func waitCopyInspection(t *testing.T, fake fakeCopySocket) {
	t.Helper()
	select {
	case err := <-fake.inspections:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fake agent did not inspect copy request")
	}
}

func assertNoCopyRequest(t *testing.T, fake fakeCopySocket) {
	t.Helper()
	select {
	case line := <-fake.requests:
		t.Fatalf("fake agent received unexpected request %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}

func copyArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "copy-") ||
			strings.HasPrefix(entry.Name(), ".copy.tmp.") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestParseClipCopyArgs(t *testing.T) {
	accepted := []struct {
		args []string
		want clipCopyArgs
	}{
		{[]string{"text"}, clipCopyArgs{kind: "text"}},
		{[]string{"text", "--trim"}, clipCopyArgs{kind: "text", trim: true}},
		{[]string{"text", "--empty-clears"}, clipCopyArgs{kind: "text", emptyClears: true}},
		{[]string{"image", "png"}, clipCopyArgs{kind: "image", format: "png"}},
		{[]string{"clear"}, clipCopyArgs{kind: "clear"}},
	}
	for _, tc := range accepted {
		got, ok := parseClipCopyArgs(tc.args)
		if !ok || got != tc.want {
			t.Errorf("parseClipCopyArgs(%q) = %+v, %v, want %+v, true", tc.args, got, ok, tc.want)
		}
	}

	rejected := [][]string{
		nil,
		{"--trim"},
		{"text", "extra"},
		{"--trim", "text"},
		{"image"},
		{"image", "jpeg"},
		{"clear", "--trim"},
		{"copy"},
	}
	for _, args := range rejected {
		if got, ok := parseClipCopyArgs(args); ok {
			t.Errorf("parseClipCopyArgs(%q) = %+v, true, want rejection", args, got)
		}
	}
}

func TestTrimOneTrailingNewline(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"a\n", "a"},
		{"a", "a"},
		{"a\n\n", "a\n"},
		{"\n", ""},
		{"", ""},
		{"a\r\n", "a\r"},
	}
	for _, tc := range tests {
		if got := string(trimOneTrailingNewline([]byte(tc.in))); got != tc.want {
			t.Errorf("trimOneTrailingNewline(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClipCopyLineFraming(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef"
	tests := []struct {
		kind   string
		format string
		size   int
		want   string
	}{
		{"text", "", 42, "copy\ttext\t" + sha + "\t42\n"},
		{"image", "png", 57, "copy\timage\tpng\t" + sha + "\t57\n"},
		{"clear", "", 0, "copy\tclear\n"},
	}
	for _, tc := range tests {
		if got := clipCopyLine(tc.kind, tc.format, sha, tc.size); got != tc.want {
			t.Errorf("clipCopyLine(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestParseCopyReply(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		readErr error
		want    bool
	}{
		{"ok", "ok\n", nil, true},
		{"truncated ok", "ok", io.EOF, false},
		{"unframed ok", "ok", nil, false},
		{"embedded carriage return", "ok\rjunk\n", nil, false},
		{"crlf", "ok\r\n", nil, false},
		{"payload", "ok\timage\n", nil, false},
		{"leading space", " ok\n", nil, false},
		{"uppercase", "OK\n", nil, false},
		{"none", "none\n", nil, false},
		{"rejected", "rejected\n", nil, false},
		{"dropped", "dropped\n", nil, false},
		{"no client", "no-client\n", nil, false},
		{"empty line", "\n", nil, false},
		{"eof", "", io.EOF, false},
		{"error with complete line", "ok\n", os.ErrDeadlineExceeded, false},
		{"buffer full", strings.Repeat("x", 256), bufio.ErrBufferFull, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCopyReply(tc.raw, tc.readErr); got != tc.want {
				t.Fatalf("parseCopyReply(%q, %v) = %v, want %v", tc.raw, tc.readErr, got, tc.want)
			}
		})
	}
}

func TestProductionClipCopyRuntimeUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := productionClipCopyRuntime().clipDir, clipDir(home); got != want {
		t.Fatalf("clip dir = %q, want HOME-derived %q", got, want)
	}

	t.Setenv("HOME", "")
	if got := productionClipCopyRuntime().clipDir; got != "" {
		t.Fatalf("clip dir with empty HOME = %q, want empty", got)
	}
}

func TestWriteCopyFileAtomic(t *testing.T) {
	sha := clipupload.ShortSHA([]byte("clipboard"))

	t.Run("created directory and exact content", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clip")
		data := []byte("clipboard")
		path, err := writeCopyFile(dir, sha, ".txt", data)
		if err != nil {
			t.Fatal(err)
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("dir mode = %04o, want 0700", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %04o, want 0600", got)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("content = %q, want %q", got, data)
		}
		if artifacts := copyArtifacts(t, dir); len(artifacts) != 1 || artifacts[0] != filepath.Base(path) {
			t.Fatalf("copy artifacts = %v, want only %q", artifacts, filepath.Base(path))
		}
	})

	t.Run("repairs existing directory mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clip")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := writeCopyFile(dir, sha, ".txt", []byte("clipboard")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("dir mode = %04o, want 0700", got)
		}
	})

	t.Run("removes temporary file on rename failure", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clip")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		finalPath := filepath.Join(dir, "copy-"+sha+".txt")
		if err := os.Mkdir(finalPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := writeCopyFile(dir, sha, ".txt", []byte("clipboard")); err == nil {
			t.Fatal("rename over directory unexpectedly succeeded")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".copy.tmp.") {
				t.Fatalf("temporary file left after failure: %s", entry.Name())
			}
		}
	})

	t.Run("replaces symlink without touching target", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "clip")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(base, "target")
		if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		finalPath := filepath.Join(dir, "copy-"+sha+".txt")
		if err := os.Symlink(target, finalPath); err != nil {
			t.Fatal(err)
		}
		if _, err := writeCopyFile(dir, sha, ".txt", []byte("clipboard")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(finalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("final mode = %v, want regular file", info.Mode())
		}
		targetData, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(targetData) != "secret" {
			t.Fatalf("symlink target changed to %q", targetData)
		}
	})
}

func TestLeaseCopyFileKeepsConcurrentIdenticalCopyAlive(t *testing.T) {
	dir := t.TempDir()
	data := []byte("clipboard")
	sha := clipupload.ShortSHA(data)

	path, releaseFirst, err := leaseCopyFile(dir, sha, ".txt", data)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, releaseSecond, err := leaseCopyFile(dir, sha, ".txt", data)
	if err != nil {
		releaseFirst()
		t.Fatal(err)
	}
	if secondPath != path {
		t.Fatalf("identical copy path = %q, want %q", secondPath, path)
	}

	releaseFirst()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("shared path after first release = %q, %v", got, err)
	}
	releaseSecond()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared path after final release: %v", err)
	}
	if leases, err := filepath.Glob(filepath.Join(dir, ".copy.lease.*")); err != nil || len(leases) != 0 {
		t.Fatalf("leases after final release = %v, %v", leases, err)
	}
}

func TestGCStaleCopies(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	names := []string{
		"copy-0123456789abcdef0123456789abcdef.txt",
		"copy-fedcba9876543210fedcba9876543210.png",
		"clip-0123456789abcdef0123456789abcdef.png",
		"text-0123456789abcdef0123456789abcdef.txt",
		".copy.tmp.stale",
		".copy.tmp.fresh",
		".copy.lease.0123456789abcdef0123456789abcdef.txt.stale",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{names[0], names[2], names[3], names[4], names[6]} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	gcStaleCopies(dir, now, time.Hour)
	for _, name := range []string{names[0], names[4], names[6]} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale %s stat error = %v, want not-exist", name, err)
		}
	}
	for _, name := range []string{names[1], names[2], names[3], names[5]} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("preserved %s: %v", name, err)
		}
	}
	gcStaleCopies(filepath.Join(dir, "missing"), now, time.Hour)
}

func TestClipCopyTimeoutBudget(t *testing.T) {
	if clipDialTimeout != 2*time.Second {
		t.Fatalf("clip dial timeout = %v, want 2s", clipDialTimeout)
	}
	if clipReadTimeout != 13*time.Second {
		t.Fatalf("clip read timeout = %v, want 13s", clipReadTimeout)
	}
}

func TestRunClipCopy_StalledSocketTimesOutAndUnlinks(t *testing.T) {
	socketDir := shortCopySocketDir(t)
	fake := startStalledCopySocket(t, socketDir, "cmd-stalled.sock")
	defer fake.stop()
	rt := clipCopyRuntime{
		stdin:       strings.NewReader("clipboard"),
		stderr:      io.Discard,
		clipDir:     filepath.Join(socketDir, "clip"),
		sockets:     func() []string { return []string{fake.path} },
		now:         time.Now,
		dialTimeout: 100 * time.Millisecond,
		readTimeout: 20 * time.Millisecond,
	}

	start := time.Now()
	if code := runClipCopy([]string{"text"}, rt); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stalled socket timeout took %v", elapsed)
	}
	if line := waitCopyRequest(t, fake); !strings.HasPrefix(line, "copy\ttext\t") {
		t.Fatalf("request = %q, want copy text framing", line)
	}
	if artifacts := copyArtifacts(t, rt.clipDir); len(artifacts) != 0 {
		t.Fatalf("copy artifacts after timeout = %v", artifacts)
	}
}

func TestClipCopyMultiClientRefusedWithoutSending(t *testing.T) {
	socketDir := shortCopySocketDir(t)
	first := startFakeCopySocket(t, socketDir, "cmd-one.sock", "ok\n")
	defer first.stop()
	second := startFakeCopySocket(t, socketDir, "cmd-two.sock", "ok\n")
	defer second.stop()

	var stderr bytes.Buffer
	rt := clipCopyRuntime{
		stdin:   strings.NewReader("clipboard"),
		stderr:  &stderr,
		clipDir: filepath.Join(socketDir, "clip"),
		sockets: func() []string { return []string{first.path, second.path} },
		now:     time.Now,
	}
	if code := runClipCopy([]string{"text"}, rt); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	assertNoCopyRequest(t, first)
	assertNoCopyRequest(t, second)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if artifacts := copyArtifacts(t, rt.clipDir); len(artifacts) != 0 {
		t.Fatalf("copy artifacts after refusal = %v", artifacts)
	}
}

func TestClipCopyReplyMapping(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  int
	}{
		{"ok", "ok\n", 0},
		{"none", "none\n", 1},
		{"rejected old agent", "rejected\n", 1},
		{"dropped", "dropped\n", 1},
		{"no client", "no-client\n", 1},
		{"eof", "", 1},
		{"payload", "ok\timage\n", 1},
		{"truncated ok", "ok", 1},
		{"crlf", "ok\r\n", 1},
		{"embedded rejected", "ok\rrejected\n", 1},
		{"buffer full", strings.Repeat("x", 300), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortCopySocketDir(t)
			fake := startFakeCopySocket(t, dir, "cmd-one.sock", tc.reply)
			defer fake.stop()

			answer, state := copyFanout(
				[]string{fake.path}, "copy\tclear\n",
				clipDialTimeout, clipReadTimeout,
			)
			got := 1
			if state == fanoutOneAgent && parseCopyReply(answer.raw, answer.readErr) {
				got = 0
			}
			if got != tc.want {
				t.Fatalf("reply %q mapped to %d, want %d (raw=%q err=%v)", tc.reply, got, tc.want, answer.raw, answer.readErr)
			}
			if line := waitCopyRequest(t, fake); line != "copy\tclear\n" {
				t.Fatalf("request = %q, want copy clear", line)
			}
		})
	}
}

func TestRunClipCopy_TextHappyPath(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	data := []byte("clipboard text\n")
	wantSHA := clipupload.ShortSHA(data)
	fake := startFakeCopySocketWithInspect(
		t, cacheDir, "cmd-copy.sock", "ok\n",
		func(line string) error {
			fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
			if len(fields) != 4 || fields[0] != "copy" || fields[1] != "text" {
				return fmt.Errorf("request = %q, want copy text framing", line)
			}
			if fields[2] != wantSHA {
				return fmt.Errorf("announced sha = %q, want %q", fields[2], wantSHA)
			}
			size, err := strconv.Atoi(fields[3])
			if err != nil {
				return fmt.Errorf("parse size %q: %w", fields[3], err)
			}
			path := filepath.Join(clipDir(home), "copy-"+fields[2]+".txt")
			got, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read copy file before reply: %w", err)
			}
			if !bytes.Equal(got, data) || size != len(data) {
				return fmt.Errorf("copy file/size = %q/%d, want %q/%d", got, size, data, len(data))
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0o600 {
				return fmt.Errorf("copy mode = %04o, want 0600", info.Mode().Perm())
			}
			return nil
		},
	)
	defer fake.stop()

	stdout, stderr, code := runClipCopyBin(t, bin, home, data, "text")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("output = stdout %q stderr %q, want both empty", stdout, stderr)
	}
	waitCopyInspection(t, fake)
	if artifacts := copyArtifacts(t, clipDir(home)); len(artifacts) != 0 {
		t.Fatalf("copy artifacts after response = %v", artifacts)
	}
}

func TestRunClipCopy_TextTrim(t *testing.T) {
	src := buildPortald(t)
	tests := []struct {
		name string
		args []string
		in   []byte
		want []byte
	}{
		{"trim", []string{"text", "--trim"}, []byte("hello\n"), []byte("hello")},
		{"preserve", []string{"text"}, []byte("hello\n"), []byte("hello\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := setupClipHome(t, src)
			cacheDir := filepath.Join(home, ".cache", "portal")
			wantSHA := clipupload.ShortSHA(tc.want)
			fake := startFakeCopySocketWithInspect(
				t, cacheDir, "cmd-copy.sock", "ok\n",
				func(line string) error {
					wantLine := clipCopyLine("text", "", wantSHA, len(tc.want))
					if line != wantLine {
						return fmt.Errorf("request = %q, want %q", line, wantLine)
					}
					got, err := os.ReadFile(filepath.Join(clipDir(home), "copy-"+wantSHA+".txt"))
					if err != nil {
						return err
					}
					if !bytes.Equal(got, tc.want) {
						return fmt.Errorf("copy bytes = %q, want %q", got, tc.want)
					}
					return nil
				},
			)
			defer fake.stop()

			stdout, stderr, code := runClipCopyBin(t, bin, home, tc.in, tc.args...)
			if code != 0 || len(stdout) != 0 || len(stderr) != 0 {
				t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			waitCopyInspection(t, fake)
		})
	}
}

func TestRunClipCopy_EmptyStdinPolicy(t *testing.T) {
	src := buildPortald(t)
	tests := []struct {
		name     string
		args     []string
		in       []byte
		wantCode int
		wantLine string
	}{
		{"text empty rejected", []string{"text"}, nil, 1, ""},
		{"trimmed empty rejected", []string{"text", "--trim"}, []byte("\n"), 1, ""},
		{"pbcopy empty clears", []string{"text", "--empty-clears"}, nil, 0, "copy\tclear\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := setupClipHome(t, src)
			cacheDir := filepath.Join(home, ".cache", "portal")
			fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", "ok\n")
			defer fake.stop()

			stdout, stderr, code := runClipCopyBin(t, bin, home, tc.in, tc.args...)
			if code != tc.wantCode || len(stdout) != 0 || len(stderr) != 0 {
				t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if tc.wantLine == "" {
				assertNoCopyRequest(t, fake)
			} else if line := waitCopyRequest(t, fake); line != tc.wantLine {
				t.Fatalf("request = %q, want %q", line, tc.wantLine)
			}
			if artifacts := copyArtifacts(t, clipDir(home)); len(artifacts) != 0 {
				t.Fatalf("copy artifacts = %v, want none", artifacts)
			}
		})
	}
}

func TestRunClipCopy_ImagePNG(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	data := append(append([]byte(nil), pngMagic...), []byte("png body")...)
	wantSHA := clipupload.ShortSHA(data)
	fake := startFakeCopySocketWithInspect(
		t, cacheDir, "cmd-copy.sock", "ok\n",
		func(line string) error {
			wantLine := clipCopyLine("image", "png", wantSHA, len(data))
			if line != wantLine {
				return fmt.Errorf("request = %q, want %q", line, wantLine)
			}
			got, err := os.ReadFile(filepath.Join(clipDir(home), "copy-"+wantSHA+".png"))
			if err != nil {
				return err
			}
			if !bytes.Equal(got, data) {
				return fmt.Errorf("copy bytes = %x, want %x", got, data)
			}
			return nil
		},
	)
	defer fake.stop()

	stdout, stderr, code := runClipCopyBin(t, bin, home, data, "image", "png")
	if code != 0 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	waitCopyInspection(t, fake)
}

func TestRunClipCopy_ImageWrongMagicNeverLeaves(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", "ok\n")
	defer fake.stop()

	stdout, stderr, code := runClipCopyBin(t, bin, home, []byte("NOTAPNGFILE!"), "image", "png")
	if code != 1 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertNoCopyRequest(t, fake)
	if artifacts := copyArtifacts(t, clipDir(home)); len(artifacts) != 0 {
		t.Fatalf("copy artifacts = %v, want none", artifacts)
	}
}

func TestRunClipCopy_OversizeNoSocketRoundTrip(t *testing.T) {
	src := buildPortald(t)
	data := bytes.Repeat([]byte{'x'}, clipupload.MaxUploadBytes+1)

	for _, args := range [][]string{{"text"}, {"text", "--empty-clears"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home, bin := setupClipHome(t, src)
			cacheDir := filepath.Join(home, ".cache", "portal")
			fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", "ok\n")
			defer fake.stop()

			stdout, stderr, code := runClipCopyBin(t, bin, home, data, args...)
			if code != 1 || len(stdout) != 0 || len(stderr) != 0 {
				t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			assertNoCopyRequest(t, fake)
		})
	}
}

func TestRunClipCopy_TruncatedOkIsFailure(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", "ok")
	defer fake.stop()

	stdout, stderr, code := runClipCopyBin(t, bin, home, []byte("clipboard"), "text")
	if code != 1 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if artifacts := copyArtifacts(t, clipDir(home)); len(artifacts) != 0 {
		t.Fatalf("copy artifacts after truncated reply = %v", artifacts)
	}
}

func TestRunClipCopy_NoSocket(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	stdout, stderr, code := runClipCopyBin(t, bin, home, []byte("clipboard"), "text")
	if code != 1 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestRunClipCopy_UnlinkedOnBothOutcomes(t *testing.T) {
	src := buildPortald(t)
	for _, tc := range []struct {
		name  string
		reply string
		code  int
	}{
		{"ok", "ok\n", 0},
		{"old agent rejected", "rejected\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := setupClipHome(t, src)
			cacheDir := filepath.Join(home, ".cache", "portal")
			fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", tc.reply)
			defer fake.stop()

			stdout, stderr, code := runClipCopyBin(t, bin, home, []byte("clipboard"), "text")
			if code != tc.code || len(stdout) != 0 || len(stderr) != 0 {
				t.Fatalf("result = code %d stdout %q stderr %q, want code %d", code, stdout, stderr, tc.code)
			}
			if artifacts := copyArtifacts(t, clipDir(home)); len(artifacts) != 0 {
				t.Fatalf("copy artifacts after %q = %v", tc.reply, artifacts)
			}
		})
	}
}

func TestRunClipCopy_GCSweepsStaleCopies(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	stale := filepath.Join(clipDir(home), "copy-0123456789abcdef0123456789abcdef.txt")
	readPath := filepath.Join(clipDir(home), "clip-fedcba9876543210fedcba9876543210.png")
	for _, path := range []string{stale, readPath} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stale, readPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fake := startFakeCopySocket(t, cacheDir, "cmd-copy.sock", "ok\n")
	defer fake.stop()

	stdout, stderr, code := runClipCopyBin(t, bin, home, nil, "clear")
	if code != 0 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale copy stat error = %v, want not-exist", err)
	}
	if _, err := os.Stat(readPath); err != nil {
		t.Fatalf("read-path file was removed: %v", err)
	}
}

func TestRunClipCopy_Usage(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	want := "usage: portald clip copy <text [--trim|--empty-clears]|image png|clear>\n"
	for _, args := range [][]string{
		nil,
		{"image", "jpeg"},
	} {
		stdout, stderr, code := runClipCopyBin(t, bin, home, nil, args...)
		if code != 1 || len(stdout) != 0 || string(stderr) != want {
			t.Errorf("args %q = code %d stdout %q stderr %q, want 1/empty/%q", args, code, stdout, stderr, want)
		}
	}
}
