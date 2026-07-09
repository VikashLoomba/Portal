//go:build darwin

package termx

import "golang.org/x/sys/unix"

const (
	getTermiosReq = unix.TIOCGETA
	setTermiosReq = unix.TIOCSETA
)
