package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/VikashLoomba/Portal/internal/clipupload"
)

const (
	clipCopyReplyLimit = 256
	clipCopyMaxAge     = time.Hour
)

type clipCopyArgs struct {
	kind        string
	format      string
	trim        bool
	emptyClears bool
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
	case len(args) == 2 && args[0] == "text" && args[1] == "--empty-clears":
		return clipCopyArgs{kind: "text", emptyClears: true}, true
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
		fmt.Fprintln(rt.stderr, "usage: portald clip copy <text [--trim|--empty-clears]|image png|clear>")
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
				if req.emptyClears {
					goto send
				}
				return 1
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
		_, release, err := leaseCopyFile(rt.clipDir, sha, ext, data)
		if err != nil {
			return 1
		}
		defer release()
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

// leaseCopyFile keeps the shared content-addressed path alive until every
// identical invocation has finished its socket round trip.
func leaseCopyFile(dir, sha, ext string, data []byte) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, err
	}

	lock, err := lockCopyDir(dir)
	if err != nil {
		return "", nil, err
	}
	defer unlockCopyDir(lock)

	path := filepath.Join(dir, "copy-"+sha+ext)
	info, statErr := os.Lstat(path)
	var existing []byte
	var readErr error
	if statErr == nil && info.Mode().IsRegular() {
		existing, readErr = os.ReadFile(path)
	}
	if statErr != nil || !info.Mode().IsRegular() || readErr != nil || !bytes.Equal(existing, data) {
		if _, err := writeCopyFile(dir, sha, ext, data); err != nil {
			return "", nil, err
		}
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}

	lease, err := os.CreateTemp(dir, ".copy.lease."+sha+ext+".*")
	if err != nil {
		return "", nil, err
	}
	leasePath := lease.Name()
	if err := lease.Close(); err != nil {
		_ = os.Remove(leasePath)
		return "", nil, err
	}

	release := func() {
		lock, err := lockCopyDir(dir)
		if err != nil {
			return
		}
		defer unlockCopyDir(lock)

		_ = os.Remove(leasePath)
		others, _ := filepath.Glob(filepath.Join(dir, ".copy.lease."+sha+ext+".*"))
		if len(others) != 0 {
			return
		}
		currentInfo, err := os.Lstat(path)
		if err == nil && os.SameFile(currentInfo, pathInfo) {
			_ = os.Remove(path)
		}
	}
	return path, release, nil
}

func lockCopyDir(dir string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(dir, ".copy.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func unlockCopyDir(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
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
	lock, err := lockCopyDir(dir)
	if err != nil {
		return
	}
	defer unlockCopyDir(lock)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() ||
			(!strings.HasPrefix(name, ".copy.tmp.") &&
				!strings.HasPrefix(name, ".copy.lease.")) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}

	entries, err = os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "copy-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		leases, _ := filepath.Glob(filepath.Join(dir,
			".copy.lease."+strings.TrimPrefix(name, "copy-")+".*"))
		if len(leases) == 0 {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// copyFanout separates connection discovery from the write. With multiple
// live agents, every connection is closed before any Mac sees a copy request.
func copyFanout(sockets []string, line string, dialTimeout, readTimeout time.Duration) (singleAgentReply, singleAgentFanoutState) {
	conns := make([]net.Conn, 0, len(sockets))
	deadline := time.Now().Add(readTimeout)
	for _, socket := range sockets {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		conn, err := net.DialTimeout("unix", socket, min(dialTimeout, remaining))
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
	if err := conn.SetDeadline(deadline); err != nil {
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
