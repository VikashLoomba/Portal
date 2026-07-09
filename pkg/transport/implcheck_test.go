package transport_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

var _ transport.Impl = transport.ImplSystemSSH
var _ transport.Impl = transport.ImplNativeSSH
var _ transport.Impl = transport.ImplLocalExec
var _ transport.Impl = transport.ImplUnavailable
var _ transport.Impl = transport.Desc{}.Impl

func TestImplVocabularyGuard(t *testing.T) {
	root := moduleRoot(t)
	assertNoBareImplStringsOutsideTransport(t, root)
	assertNoConformanceImplBranches(t, root)
}

func assertNoBareImplStringsOutsideTransport(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".go-cache", "node_modules", "clients", "scratchpad":
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
		relSlash := filepath.ToSlash(rel)
		if strings.HasPrefix(relSlash, "pkg/transport/") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		checkImplStringLiterals(t, relSlash, src, fset, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

func checkImplStringLiterals(t *testing.T, rel string, src []byte, fset *token.FileSet, file *ast.File) {
	t.Helper()
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		switch value {
		case string(transport.ImplSystemSSH), string(transport.ImplNativeSSH):
			pos := fset.Position(lit.Pos())
			t.Errorf("%s:%d uses bare impl string %q outside pkg/transport", rel, pos.Line, value)
		case string(transport.ImplLocalExec), string(transport.ImplUnavailable):
			if inImplContext(src, fset, stack, lit) {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d uses bare impl string %q in Impl context outside pkg/transport", rel, pos.Line, value)
			}
		}
		return true
	})
}

func inImplContext(src []byte, fset *token.FileSet, stack []ast.Node, lit *ast.BasicLit) bool {
	for _, n := range stack {
		if kv, ok := n.(*ast.KeyValueExpr); ok && nodeContains(kv.Value, lit) {
			if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == "Impl" {
				return true
			}
		}
		if be, ok := n.(*ast.BinaryExpr); ok && (be.Op == token.EQL || be.Op == token.NEQ) {
			switch {
			case nodeContains(be.X, lit):
				if strings.Contains(nodeSource(src, fset, be.Y), "Impl") {
					return true
				}
			case nodeContains(be.Y, lit):
				if strings.Contains(nodeSource(src, fset, be.X), "Impl") {
					return true
				}
			}
		}
	}
	return false
}

func assertNoConformanceImplBranches(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "pkg", "transport", "conformance")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "Describe().Impl") {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			t.Errorf("%s contains Describe().Impl", filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk conformance: %v", err)
	}
}

func nodeContains(root ast.Node, child ast.Node) bool {
	return root != nil && child.Pos() >= root.Pos() && child.End() <= root.End()
}

func nodeSource(src []byte, fset *token.FileSet, n ast.Node) string {
	if n == nil {
		return ""
	}
	start := fset.Position(n.Pos()).Offset
	end := fset.Position(n.End()).Offset
	if start < 0 || end < start || end > len(src) {
		return ""
	}
	return string(src[start:end])
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
