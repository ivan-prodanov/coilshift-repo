package supervise

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The Summon code's signals (CR-1). A test that signals a pid it read from a file, or one it never set, can send
// kill(0, sig) to its own process group (go test's, and the shell's), kill(-1, sig) to every process it may signal,
// or a signal to init; an earlier test harness on this host tried to signal real host pids. So:
//
//   - production code in these packages never calls kill(2) itself: its one signal is supervise.Run's SIGKILL to the
//     child it started (cmd.Process, which os/exec gave a real pid, and which refuses pid 0 and -1 itself), and
//     nothing turns a pid into an *os.Process (os.FindProcess);
//   - a test signals only through its package's signalPID, which refuses pid <= 1 and sends nothing.
//
// killDirs are the Summon packages, from the module root, each with everything under it.
var killDirs = []string{"cmd/agw", "cmd/summonhelper", "capability/summonrelay", "client", "internal/carlink",
	"internal/peercred", "internal/summon", "internal/supervise"}

// killCalls are the calls that signal a pid, by package path: kill(2), tkill, tgkill, a pidfd's signal, and
// os.FindProcess, which turns any pid into a Process to signal.
var killCalls = map[string]map[string]bool{
	"syscall":               {"Kill": true, "Tgkill": true, "Tkill": true},
	"golang.org/x/sys/unix": {"Kill": true, "Tgkill": true, "Tkill": true, "PidfdSendSignal": true},
	"os":                    {"FindProcess": true},
}

func TestNoUnguardedKill(t *testing.T) {
	root := moduleRoot(t)
	files := 0
	for _, dir := range killDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			files++
			for _, bad := range unguardedKills(t, path) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%s", rel, bad)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if files < 50 {
		t.Fatalf("checked only %d Go files under %v: the walk is not reading the packages", files, killDirs)
	}
}

// unguardedKills lists the calls in one file that signal a pid outside the rule: in production code any such call, in
// a test file any outside a function named signalPID.
func unguardedKills(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	names := map[string]string{} // the file's local name of each package in killCalls → its path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if _, ok := killCalls[p]; !ok {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = p
	}
	if len(names) == 0 {
		return nil
	}
	test := strings.HasSuffix(path, "_test.go")
	var bad []string
	for _, decl := range f.Decls {
		inGuard := false
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "signalPID" {
			inGuard = test
			if test && !guardsFirst(fn) {
				bad = append(bad, strconv.Itoa(fset.Position(fn.Pos()).Line)+": signalPID must start with "+
					"`if pid <= 1 { return ... }`, before it signals anything")
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if p, ok := names[x.Name]; ok && killCalls[p][sel.Sel.Name] && !inGuard {
				where := "production code never signals a pid itself"
				if test {
					where = "a test signals only through signalPID, which refuses pid <= 1"
				}
				bad = append(bad, strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+x.Name+"."+sel.Sel.Name+": "+where)
			}
			return true
		})
	}
	return bad
}

// guardsFirst reports whether fn's first parameter is pid and its first statement is `if pid <= 1 { ...; return ... }`.
func guardsFirst(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) == 0 || len(params[0].Names) == 0 || params[0].Names[0].Name != "pid" || fn.Body == nil ||
		len(fn.Body.List) == 0 {
		return false
	}
	ifs, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || len(ifs.Body.List) == 0 {
		return false
	}
	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.LEQ {
		return false
	}
	x, xok := cond.X.(*ast.Ident)
	y, yok := cond.Y.(*ast.BasicLit)
	_, ret := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt)
	return xok && x.Name == "pid" && yok && y.Value == "1" && ret
}

// moduleRoot is the directory holding go.mod, above this package's.
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
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = up
	}
}

// signalPID itself: pid 0, -1 and 1 are refused and nothing is sent. Signal 0 sends nothing even unguarded, so this
// cannot reach another process whatever the guard does. This test binary's own pid passes.
func TestSignalPIDRefusesGroupsAndInit(t *testing.T) {
	for _, pid := range []int{0, -1, 1, -os.Getpid()} {
		if err := signalPID(pid, 0); err == nil {
			t.Errorf("signalPID(%d, 0) passed: kill(2) with it reaches a group, every process, or init", pid)
		}
	}
	if err := signalPID(os.Getpid(), 0); err != nil && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signalPID(this pid, 0) = %v", err)
	}
	if alive(0) || alive(-1) || alive(1) {
		t.Fatal("alive is true for pid 0, -1 or 1")
	}
}
