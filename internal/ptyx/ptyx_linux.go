//go:build linux

package ptyx

import (
	"fmt"
	"os"
	"strconv"

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
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}

	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, "", fmt.Errorf("get pty number: %w", err)
	}

	ok = true
	return master, "/dev/pts/" + strconv.Itoa(n), nil
}
