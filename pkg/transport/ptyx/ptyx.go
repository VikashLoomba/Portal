package ptyx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	defaultRows uint16 = 24
	defaultCols uint16 = 80
)

func Start(cmd *exec.Cmd, rows, cols uint16) (master *os.File, err error) {
	if cmd == nil {
		return nil, errors.New("ptyx: nil command")
	}

	master, slaveName, err := open()
	if err != nil {
		return nil, err
	}

	slave, err := os.OpenFile(slaveName, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("open pty slave %q: %w", slaveName, err)
	}

	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}

	if err := Setsize(master, rows, cols); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, fmt.Errorf("set pty size: %w", err)
	}

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}

	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, err
	}
	if err := slave.Close(); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("close pty slave: %w", err)
	}

	return master, nil
}

func Setsize(f *os.File, rows, cols uint16) error {
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows,
		Col: cols,
	})
}

func Getsize(f *os.File) (rows, cols uint16, err error) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return ws.Row, ws.Col, nil
}
