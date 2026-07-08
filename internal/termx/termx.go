package termx

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, getTermiosReq)
	return err == nil
}

func MakeRaw(fd int) (restore func() error, err error) {
	saved, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		return nil, err
	}

	raw := *saved
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, setTermiosReq, &raw); err != nil {
		return nil, err
	}

	original := *saved
	return func() error {
		return unix.IoctlSetTermios(fd, setTermiosReq, &original)
	}, nil
}

func GetSize(fd int) (rows, cols uint16, err error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return ws.Row, ws.Col, nil
}

func WatchWinch(ctx context.Context) <-chan struct{} {
	sig := make(chan os.Signal, 1)
	out := make(chan struct{}, 1)
	signal.Notify(sig, unix.SIGWINCH)

	go func() {
		defer close(out)
		defer signal.Stop(sig)

		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()

	return out
}
