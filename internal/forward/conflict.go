package forward

// conflictSet tracks ports we've already logged a "SKIP — local port held by
// foreign process" message for. It is the ONLY in-memory state in the engine
// and is NON-AUTHORITATIVE: it merely suppresses repeated log lines. The
// forwarding source of truth is ALWAYS PortLister.MasterForwards(pid). Do
// NOT add a forwarded-ports cache here — it would break the self-healing
// stateless reconcile after a master rebuild.
type conflictSet struct {
	m map[int]struct{}
}

func newConflictSet() *conflictSet { return &conflictSet{m: map[int]struct{}{}} }

// note logs SKIP once for (port, holder, name) and remembers we did. The
// name is provided lazily (a thunk) so the engine doesn't shell out to `ps`
// when this conflict has already been logged — matching the bash original
// where note_conflict early-returns BEFORE `ps -o comm=`.
func (s *conflictSet) note(port, holderPID int, nameFn func() string, log Logger) {
	if _, ok := s.m[port]; ok {
		return
	}
	s.m[port] = struct{}{}
	name := ""
	if nameFn != nil {
		name = nameFn()
	}
	if name == "" {
		log.Logf("SKIP port %d: local port already in use by pid %d (pid %d)", port, holderPID, holderPID)
		return
	}
	log.Logf("SKIP port %d: local port already in use by %s (pid %d)", port, name, holderPID)
}

func (s *conflictSet) clear(port int) { delete(s.m, port) }

// prune drops any conflict entries no longer in the desired set (so an
// unchanged conflict is logged again only when the port reappears).
func (s *conflictSet) prune(desired []int) {
	keep := make(map[int]struct{}, len(desired))
	for _, p := range desired {
		keep[p] = struct{}{}
	}
	for p := range s.m {
		if _, ok := keep[p]; !ok {
			delete(s.m, p)
		}
	}
}
