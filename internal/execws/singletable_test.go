package execws

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestSingleWebSocketOpcodeTable(t *testing.T) {
	root := moduleRoot(t)
	opConst := regexp.MustCompile(`Op(Continuation|Text|Binary|Close|Ping|Pong)\b.*=\s*0x`)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".go-cache":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "internal/execws/framing.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.Contains(src, "wsOpcode") || strings.Contains(src, "execWSOpcode") {
			t.Errorf("%s contains a deleted websocket opcode type name", rel)
		}
		if opConst.MatchString(src) {
			t.Errorf("%s declares websocket opcode constants outside internal/execws", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test file")
		}
		dir = parent
	}
}
