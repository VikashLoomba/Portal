//go:build darwin

package localapi

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID extracts the connecting peer's uid via LOCAL_PEERCRED on darwin. The
// getsockopt runs inside RawConn.Control so the fd stays valid for the call.
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var operr error
	if err := raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			operr = e
			return
		}
		uid = int(cred.Uid)
	}); err != nil {
		return 0, err
	}
	if operr != nil {
		return 0, operr
	}
	return uid, nil
}
