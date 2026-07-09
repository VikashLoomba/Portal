package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/VikashLoomba/Portal/internal/"

type goListPackage struct {
	ImportPath string
	Imports    []string
}

// EC10: public pkg packages must not import internal packages in non-test imports.
func TestPkgPackagesDoNotImportInternal(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-json", "./pkg/...")
	cmd.Dir = root
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -json ./pkg/...: %v\n%s", err, stderr.String())
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode go list package: %v", err)
		}
		for _, imp := range pkg.Imports {
			if strings.HasPrefix(imp, internalImportPrefix) {
				t.Errorf("%s imports internal package %s", pkg.ImportPath, imp)
			}
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
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
