package localexec

import (
	"context"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

// shQuote mirrors the install/doctor/conformance single-quote wrap so the
// shell-join test drives the exact same quoting path as production consumers.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestDescribe(t *testing.T) {
	got := New().Describe()
	want := transport.Desc{Impl: "localexec", Host: "local", Endpoint: ""}
	if got != want {
		t.Errorf("Describe = %+v, want %+v", got, want)
	}
}

func TestHealth(t *testing.T) {
	h, err := New().Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := transport.Health{Up: true, Pid: 0, Detail: "localexec"}
	if h != want {
		t.Errorf("Health = %+v, want %+v", h, want)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	l := New()
	ctx := context.Background()
	if rebuilt, err := l.Ensure(ctx); err != nil || rebuilt {
		t.Fatalf("Ensure #1 = (%v, %v), want (false, nil)", rebuilt, err)
	}
	if rebuilt, err := l.Ensure(ctx); err != nil || rebuilt {
		t.Fatalf("Ensure #2 = (%v, %v), want (false, nil)", rebuilt, err)
	}
}

func TestNonZeroExitErrorShape(t *testing.T) {
	_, _, err := New().Exec(context.Background(), nil, "sh", "-c", shQuote("exit 5"))
	if err == nil {
		t.Fatal("want error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "5") {
		t.Errorf("error = %q, want to mention exit code 5", err.Error())
	}
}

// TestShellJoin proves localexec joins argv and runs `sh -c <joined>` rather
// than direct-execing argv[0]: a direct-exec impl would try to run a program
// literally named `sh` with a quoted script arg and never expand the `;`,
// `>&2`, or `exit`. The quoted multi-statement script must produce stdout
// `out`, stderr `err`, and an error mentioning the exit code 3.
func TestShellJoin(t *testing.T) {
	stdout, stderr, err := New().Exec(context.Background(), nil,
		"sh", "-c", shQuote("echo out; echo err >&2; exit 3"))
	if err == nil {
		t.Fatal("want error for exit 3")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error = %q, want to mention 3", err.Error())
	}
	if strings.TrimSpace(stdout) != "out" {
		t.Errorf("stdout = %q, want \"out\"", stdout)
	}
	if strings.TrimSpace(stderr) != "err" {
		t.Errorf("stderr = %q, want \"err\"", stderr)
	}
}
