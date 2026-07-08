//go:build darwin

package ptyx

import (
	"bytes"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func open() (master *os.File, slaveName string, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}

	ok := false
	defer func() {
		if !ok {
			_ = master.Close()
		}
	}()

	fd := int(master.Fd())
	if err := ioctl(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return nil, "", fmt.Errorf("grant pty: %w", err)
	}
	if err := ioctl(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}

	var name [128]byte
	if err := ioctlPtr(fd, unix.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		return nil, "", fmt.Errorf("get pty name: %w", err)
	}

	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		end = len(name)
	}
	slaveName = string(name[:end])
	if slaveName == "" {
		return nil, "", fmt.Errorf("get pty name: empty response")
	}

	ok = true
	return master, slaveName, nil
}

func ioctl(fd int, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlPtr(fd int, req uint, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
