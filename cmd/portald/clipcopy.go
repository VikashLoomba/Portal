package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/VikashLoomba/Portal/internal/clipupload"
)

const (
	clipCopyReplyLimit = 256
	clipCopyMaxAge     = time.Hour
)

type clipCopyArgs struct {
	kind   string
	format string
	trim   bool
}

type clipCopyRuntime struct {
	stdin       io.Reader
	stderr      io.Writer
	clipDir     string
	sockets     func() []string
	now         func() time.Time
	dialTimeout time.Duration
	readTimeout time.Duration
}

func productionClipCopyRuntime() clipCopyRuntime {
	home := os.Getenv("HOME")
	dir := ""
	if home != "" {
		dir = filepath.Join(home, ".cache", "portal", "clip")
	}
	return clipCopyRuntime{
		stdin:       os.Stdin,
		stderr:      os.Stderr,
		clipDir:     dir,
		sockets:     cmdSocketEntries,
		now:         time.Now,
		dialTimeout: clipDialTimeout,
		readTimeout: clipReadTimeout,
	}
}

func parseClipCopyArgs(args []string) (clipCopyArgs, bool) {
	switch {
	case len(args) == 1 && args[0] == "text":
		return clipCopyArgs{kind: "text"}, true
	case len(args) == 2 && args[0] == "text" && args[1] == "--trim":
		return clipCopyArgs{kind: "text", trim: true}, true
	case len(args) == 2 && args[0] == "image" && args[1] == "png":
		return clipCopyArgs{kind: "image", format: "png"}, true
	case len(args) == 1 && args[0] == "clear":
		return clipCopyArgs{kind: "clear"}, true
	default:
		return clipCopyArgs{}, false
	}
}

func trimOneTrailingNewline(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return data[:len(data)-1]
	}
	return data
}

func clipCopyLine(kind, format, sha string, size int) string {
	switch kind {
	case "text":
		return "copy\ttext\t" + sha + "\t" + strconv.Itoa(size) + "\n"
	case "image":
		return "copy\timage\t" + format + "\t" + sha + "\t" + strconv.Itoa(size) + "\n"
	case "clear":
		return "copy\tclear\n"
	default:
		return ""
	}
}

// runClipCopy owns the side-channel file lifetime and returns an exit code so
// main's os.Exit cannot bypass the unlink and stale-file sweep.
func runClipCopy(args []string, rt clipCopyRuntime) int {
	req, ok := parseClipCopyArgs(args)
	if !ok {
		fmt.Fprintln(rt.stderr, "usage: portald clip copy <text [--trim]|image png|clear>")
		return 1
	}
	if rt.clipDir == "" {
		return 1
	}
	defer gcStaleCopies(rt.clipDir, rt.now(), clipCopyMaxAge)

	line := clipCopyLine("clear", "", "", 0)
	if req.kind != "clear" {
		data, err := io.ReadAll(io.LimitReader(rt.stdin, clipupload.MaxUploadBytes+1))
		if err != nil || len(data) > clipupload.MaxUploadBytes {
			return 1
		}
		if req.kind == "image" {
			if verifyPNG(data) != nil {
				return 1
			}
		} else {
			if req.trim {
				data = trimOneTrailingNewline(data)
			}
			if len(data) == 0 {
				goto send
			}
		}

		sha := clipupload.ShortSHA(data)
		if !shaRE.MatchString(sha) {
			return 1
		}
		ext := ".txt"
		if req.kind == "image" {
			ext = ".png"
		}
		path, err := writeCopyFile(rt.clipDir, sha, ext, data)
		if err != nil {
			return 1
		}
		defer os.Remove(path)
		line = clipCopyLine(req.kind, req.format, sha, len(data))
	}

send:
	dialTimeout := rt.dialTimeout
	if dialTimeout <= 0 {
		dialTimeout = clipDialTimeout
	}
	readTimeout := rt.readTimeout
	if readTimeout <= 0 {
		readTimeout = clipReadTimeout
	}
	answer, state := copyFanout(
		rt.sockets(), line, dialTimeout, readTimeout,
	)
	if state != fanoutOneAgent || !parseCopyReply(answer.raw, answer.readErr) {
		return 1
	}
	return 0
}

// writeCopyFile installs data without ever opening the content-addressed final
// path, so a planted symlink is replaced rather than followed.
func writeCopyFile(dir, sha, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".copy.tmp.*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := writeFull(tmp, data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	// Identical concurrent copies intentionally share this content address. If
	// one removes it while another Mac pulls, the second copy fails closed and
	// its shim falls through instead of reporting a write that did not land.
	path := filepath.Join(dir, "copy-"+sha+ext)
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}
	removeTemp = false
	return path, nil
}

func gcStaleCopies(dir string, now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "copy-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// copyFanout separates connection discovery from the write. With multiple
// live agents, every connection is closed before any Mac sees a copy request.
func copyFanout(sockets []string, line string, dialTimeout, readTimeout time.Duration) (singleAgentReply, singleAgentFanoutState) {
	conns := make([]net.Conn, 0, len(sockets))
	for _, socket := range sockets {
		conn, err := net.DialTimeout("unix", socket, dialTimeout)
		if err != nil {
			continue
		}
		conns = append(conns, conn)
	}

	switch len(conns) {
	case 0:
		return singleAgentReply{}, fanoutNoAgent
	case 1:
	default:
		for _, conn := range conns {
			_ = conn.Close()
		}
		return singleAgentReply{}, fanoutMultipleAgents
	}

	conn := conns[0]
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(readTimeout)); err != nil {
		return singleAgentReply{readErr: err}, fanoutOneAgent
	}
	if _, err := io.WriteString(conn, line); err != nil {
		return singleAgentReply{readErr: err}, fanoutOneAgent
	}
	buf, err := bufio.NewReaderSize(conn, clipCopyReplyLimit).ReadSlice('\n')
	return singleAgentReply{raw: string(buf), readErr: err}, fanoutOneAgent
}

func parseCopyReply(raw string, readErr error) bool {
	if readErr != nil {
		return false
	}
	line, ok := strings.CutSuffix(raw, "\n")
	return ok && line == "ok"
}
