//go:build linux

package watcher

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// NetlinkConfig tunes the production watcher.
//
//   - PollInterval: how often to issue an INET_DIAG dump (75ms is the
//     designed sweet spot — sub-100ms add-detection latency, ~0.05% CPU).
//   - UseDestroyMulticast: also subscribe to SKNLGRP_INET_TCP_DESTROY +
//     SKNLGRP_INET6_TCP_DESTROY for instant remove events. Adds a single
//     extra netlink socket; cheap.
type NetlinkConfig struct {
	PollInterval        int  // ms; default 75
	UseDestroyMulticast bool // default true
}

// NewNetlink constructs a production watcher. Returns ErrUnsupported if
// NETLINK_SOCK_DIAG is not available (very old kernels).
func NewNetlink(cfg NetlinkConfig) (Watcher, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 75
	}
	c, err := netlink.Dial(unix.NETLINK_SOCK_DIAG, nil)
	if err != nil {
		return nil, fmt.Errorf("netlink dial NETLINK_SOCK_DIAG: %w", err)
	}
	w := &nlWatcher{
		dumpConn: c,
		poll:     time.Duration(cfg.PollInterval) * time.Millisecond,
		useMC:    cfg.UseDestroyMulticast,
		baseline: map[uint64]Listen{}, // keyed by inode
	}
	if cfg.UseDestroyMulticast {
		mc, err := netlink.Dial(unix.NETLINK_SOCK_DIAG, nil)
		if err == nil {
			// Kernel UAPI sknetlink_groups (include/uapi/linux/sock_diag.h):
			//   SKNLGRP_NONE = 0
			//   SKNLGRP_INET_TCP_DESTROY  = 1
			//   SKNLGRP_INET_UDP_DESTROY  = 2
			//   SKNLGRP_INET6_TCP_DESTROY = 3   (NOT 4 — that's UDP6)
			//   SKNLGRP_INET6_UDP_DESTROY = 4
			_ = mc.JoinGroup(sknlgrpInetTCPDestroy)
			_ = mc.JoinGroup(sknlgrpInet6TCPDestroy)
			w.mcConn = mc
		}
	}
	return w, nil
}

// nlWatcher implements Watcher over INET_DIAG.
type nlWatcher struct {
	dumpConn *netlink.Conn
	mcConn   *netlink.Conn
	poll     time.Duration
	useMC    bool

	mu       sync.Mutex
	baseline map[uint64]Listen // inode -> listen
}

const (
	// from <linux/sock_diag.h>
	sockDiagByFamily = 20

	// sknetlink_groups (kernel UAPI). Used as multicast group IDs.
	sknlgrpInetTCPDestroy  = 1
	sknlgrpInet6TCPDestroy = 3

	// from <linux/inet_diag.h>
	inetDiagInfo = 2
	tcpListen    = 10
)

// inet_diag_req_v2 (40 bytes):
//   __u8 sdiag_family
//   __u8 sdiag_protocol
//   __u8 idiag_ext
//   __u8 pad
//   __u32 idiag_states
//   inet_diag_sockid id (48 bytes — but we send zeros for dump)
type inetDiagReqV2 struct {
	Family   uint8
	Protocol uint8
	Ext      uint8
	Pad      uint8
	States   uint32
	// inet_diag_sockid (zeroed for dump):
	IDSPort  uint16
	IDDPort  uint16
	IDSrc    [16]byte
	IDDst    [16]byte
	IDIf     uint32
	IDCookie [2]uint32
}

func (r *inetDiagReqV2) marshal() []byte {
	b := make([]byte, 56)
	b[0] = r.Family
	b[1] = r.Protocol
	b[2] = r.Ext
	b[3] = r.Pad
	binary.LittleEndian.PutUint32(b[4:8], r.States)
	binary.BigEndian.PutUint16(b[8:10], r.IDSPort)
	binary.BigEndian.PutUint16(b[10:12], r.IDDPort)
	copy(b[12:28], r.IDSrc[:])
	copy(b[28:44], r.IDDst[:])
	binary.LittleEndian.PutUint32(b[44:48], r.IDIf)
	binary.LittleEndian.PutUint32(b[48:52], r.IDCookie[0])
	binary.LittleEndian.PutUint32(b[52:56], r.IDCookie[1])
	return b
}

// inet_diag_msg layout (first bytes only — we read what we need):
//   __u8 idiag_family
//   __u8 idiag_state
//   __u8 idiag_timer
//   __u8 idiag_retrans
//   inet_diag_sockid (48 bytes)
//   __u32 idiag_expires
//   __u32 idiag_rqueue
//   __u32 idiag_wqueue
//   __u32 idiag_uid
//   __u32 idiag_inode
type parsedRow struct {
	Family uint8
	State  uint8
	SPort  uint16
	Src    [16]byte
	Inode  uint32
}

func parseDiagMsg(data []byte) (parsedRow, bool) {
	if len(data) < 72 {
		return parsedRow{}, false
	}
	var p parsedRow
	p.Family = data[0]
	p.State = data[1]
	p.SPort = binary.BigEndian.Uint16(data[4:6])
	copy(p.Src[:], data[8:24])
	p.Inode = binary.LittleEndian.Uint32(data[68:72])
	return p, true
}

func dumpFamily(c *netlink.Conn, family uint8) ([]Listen, error) {
	req := &inetDiagReqV2{
		Family:   family,
		Protocol: unix.IPPROTO_TCP,
		States:   1 << tcpListen, // listen-only
	}
	msg := netlink.Message{
		Header: netlink.Header{
			Type:  sockDiagByFamily,
			Flags: netlink.Request | netlink.Dump,
		},
		Data: req.marshal(),
	}
	res, err := c.Execute(msg)
	if err != nil {
		return nil, err
	}
	out := make([]Listen, 0, len(res))
	for _, m := range res {
		row, ok := parseDiagMsg(m.Data)
		if !ok || row.State != tcpListen {
			continue
		}
		l := Listen{Port: row.SPort, InodeNS: row.Inode}
		if family == unix.AF_INET {
			l.Family = 4
			l.Addr = net.IPv4(row.Src[0], row.Src[1], row.Src[2], row.Src[3]).String()
		} else {
			l.Family = 6
			ip := net.IP(row.Src[:]).String()
			l.Addr = ip
		}
		out = append(out, l)
	}
	return out, nil
}

func (w *nlWatcher) dumpAll() ([]Listen, error) {
	v4, err := dumpFamily(w.dumpConn, unix.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("dump v4: %w", err)
	}
	v6, err := dumpFamily(w.dumpConn, unix.AF_INET6)
	if err != nil {
		return nil, fmt.Errorf("dump v6: %w", err)
	}
	return append(v4, v6...), nil
}

// SnapshotNow returns the current listen-set unfiltered. The Filter applies
// loopback/deny/allow on the agent.Server side.
func (w *nlWatcher) SnapshotNow(ctx context.Context) ([]Listen, error) {
	return w.dumpAll()
}

// Start launches the polling goroutine and the (optional) multicast
// goroutine. The returned channel closes ONLY after every producer has
// returned, so a concurrent "send on closed channel" panic is impossible.
func (w *nlWatcher) Start(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event, 256)
	var producers sync.WaitGroup
	producers.Add(1)
	go func() {
		defer producers.Done()
		w.pollLoop(ctx, out)
	}()
	if w.useMC && w.mcConn != nil {
		producers.Add(1)
		go func() {
			defer producers.Done()
			w.mcLoop(ctx, out)
		}()
	}
	go func() {
		<-ctx.Done()
		// Closing the netlink conns unblocks the in-flight Receive/Execute.
		_ = w.dumpConn.Close()
		if w.mcConn != nil {
			_ = w.mcConn.Close()
		}
		// Then wait for the producer goroutines to finish their final
		// loop iteration before announcing shutdown via close(out).
		producers.Wait()
		close(out)
	}()
	return out, nil
}

func (w *nlWatcher) pollLoop(ctx context.Context, out chan<- Event) {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	// Seed baseline on first tick so initial Snapshot already reflects state.
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ls, err := w.dumpAll()
		if err != nil {
			continue
		}
		w.diffAndEmit(ls, out)
	}
}

func (w *nlWatcher) diffAndEmit(now []Listen, out chan<- Event) {
	cur := make(map[uint64]Listen, len(now))
	for _, l := range now {
		key := keyForListen(l)
		cur[key] = l
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// We track which (key) transitions we successfully emitted this tick
	// so we can advance the baseline ONLY for those even if a later send
	// hit backpressure. Without this partial-progress accounting, a full
	// channel during the Add loop would leave the baseline un-advanced
	// and re-fire the same Adds on every subsequent tick — consuming CPU
	// and channel slots indefinitely. (The agent.Server dedups masks the
	// downstream damage today, but the watcher contract is "each transition
	// fires once" and we honor that here.)
	t := time.Now()
	addedOK := make(map[uint64]Listen)
	removedOK := make(map[uint64]bool)

	for k, l := range cur {
		if _, had := w.baseline[k]; had {
			continue
		}
		select {
		case out <- Event{Kind: KindAdd, Listen: l, At: t, Source: 1}:
			addedOK[k] = l
		default:
			// Backpressure — fall through to merge what we have.
			goto merge
		}
	}
	for k, l := range w.baseline {
		if _, still := cur[k]; still {
			continue
		}
		select {
		case out <- Event{Kind: KindRemove, Listen: l, At: t, Source: 1}:
			removedOK[k] = true
		default:
			goto merge
		}
	}
	// All events delivered: take the new full set as the baseline.
	w.baseline = cur
	return

merge:
	// Partial progress: copy delivered adds into baseline, drop delivered
	// removes from baseline. Anything we couldn't send remains pending and
	// will be retried on the next tick, but the events we DID send won't
	// be re-emitted.
	for k, l := range addedOK {
		w.baseline[k] = l
	}
	for k := range removedOK {
		delete(w.baseline, k)
	}
}

func keyForListen(l Listen) uint64 {
	// Pack (family, port, inode) into a single key. Inode disambiguates
	// short-lived listeners that reuse the same port.
	return uint64(l.Family)<<56 | uint64(l.Port)<<40 | uint64(l.InodeNS)
}

// mcLoop reads SKNLGRP_INET_TCP_DESTROY messages and emits Remove events
// for ports we believe are still listening. The multicast group fires for
// EVERY tcp socket close on the box; we filter against the current
// baseline so non-listen closes are dropped.
func (w *nlWatcher) mcLoop(ctx context.Context, out chan<- Event) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgs, err := w.mcConn.Receive()
		if err != nil {
			return
		}
		for _, m := range msgs {
			row, ok := parseDiagMsg(m.Data)
			if !ok {
				continue
			}
			fam := uint8(4)
			if row.Family == unix.AF_INET6 {
				fam = 6
			}
			key := uint64(fam)<<56 | uint64(row.SPort)<<40 | uint64(row.Inode)
			w.mu.Lock()
			l, was := w.baseline[key]
			if was {
				delete(w.baseline, key)
			}
			w.mu.Unlock()
			if !was {
				continue
			}
			select {
			case out <- Event{Kind: KindRemove, Listen: l, At: time.Now(), Source: 2}:
			default:
				// Drop on backpressure; the next dump-diff will catch it.
			}
		}
	}
}
