package run

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// FakeCall records one observed Runner.Run invocation.
type FakeCall struct {
	Name  string
	Args  []string
	Stdin string
}

// FakeReply scripts the response Fake should return for a matching call.
// Match is a substring match against the joined "name args..." string;
// empty Match matches any call. Replies are consumed in registration order
// among matching ones.
type FakeReply struct {
	Match    string
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Fake is an in-memory Runner with scripted replies and a call log. It is
// concurrency-safe so the reconcile-loop tests can use it from a single
// goroutine while also asserting parallel-safety.
type Fake struct {
	mu      sync.Mutex
	Calls   []FakeCall
	Replies []FakeReply
	// Default is returned when no Reply matches.
	Default FakeReply
}

// AddReply appends a scripted response.
func (f *Fake) AddReply(r FakeReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Replies = append(f.Replies, r)
}

func (f *Fake) Run(_ context.Context, name string, args []string, stdin string) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: append([]string(nil), args...), Stdin: stdin})

	joined := name
	if len(args) > 0 {
		joined = name + " " + strings.Join(args, " ")
	}
	for i, r := range f.Replies {
		if r.Match == "" || strings.Contains(joined, r.Match) {
			f.Replies = append(f.Replies[:i], f.Replies[i+1:]...)
			return r.Stdout, r.Stderr, r.ExitCode, r.Err
		}
	}
	return f.Default.Stdout, f.Default.Stderr, f.Default.ExitCode, f.Default.Err
}

// String returns a debug rendering of all calls.
func (f *Fake) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for i, c := range f.Calls {
		fmt.Fprintf(&b, "%d: %s %s\n", i, c.Name, strings.Join(c.Args, " "))
	}
	return b.String()
}

var _ Runner = (*Fake)(nil)
