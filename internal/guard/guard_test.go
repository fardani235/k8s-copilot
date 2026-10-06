package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type source struct {
	rel  string // path relative to the module root, slash-separated
	file *ast.File
	fset *token.FileSet
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
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// sources parses every non-test Go file of the module.
func sources(t *testing.T) []source {
	t.Helper()
	root := moduleRoot(t)
	var out []source
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, source{rel: filepath.ToSlash(rel), file: f, fset: fset})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 15 {
		t.Fatalf("only %d source files found: the guard is not looking at the module", len(out))
	}
	return out
}

func imports(s source) []string {
	var out []string
	for _, im := range s.file.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		out = append(out, p)
	}
	return out
}

// calls returns "X.Sel" for every selector call in the file, plus the bare
// selector name.
func calls(s source, visit func(sel string, pos token.Position)) {
	ast.Inspect(s.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			visit(sel.Sel.Name, s.fset.Position(sel.Pos()))
		}
		return true
	})
}

// 5.5: no code path can shell out. Without os/exec (or the raw syscalls)
// there is no way to run kubectl or a shell, whatever the model says.
func TestNothingCanRunASubprocess(t *testing.T) {
	banned := map[string]string{
		"os/exec":   "runs subprocesses",
		"syscall":   "can exec/fork",
		"plugin":    "loads code at run time",
		"unsafe":    "defeats the type-level guarantees",
		"net/rpc":   "not needed",
		"os/signal": "not needed",
	}
	for _, s := range sources(t) {
		for _, im := range imports(s) {
			if why, bad := banned[im]; bad {
				t.Errorf("%s imports %q (%s): cluster operations must go through client-go, never a command", s.rel, im, why)
			}
		}
		calls(s, func(sel string, pos token.Position) {
			if sel == "StartProcess" || sel == "ForkExec" {
				t.Errorf("%s: call to %s", pos, sel)
			}
		})
	}
}

// Only internal/kube may hold a Kubernetes client. Everything else — the UI,
// the agent, the tools — reaches the cluster through kube.Cluster's methods.
func TestOnlyKubePackageTalksToTheAPI(t *testing.T) {
	clientPkgs := []string{
		"k8s.io/client-go/dynamic", "k8s.io/client-go/kubernetes", "k8s.io/client-go/discovery",
		"k8s.io/client-go/rest", "k8s.io/client-go/tools/clientcmd", "k8s.io/client-go/scale",
		"k8s.io/client-go/metadata", "k8s.io/client-go/transport", "k8s.io/client-go/tools/remotecommand",
		"k8s.io/client-go/tools/portforward", "k8s.io/kubectl",
	}
	for _, s := range sources(t) {
		if strings.HasPrefix(s.rel, "internal/kube/") {
			continue
		}
		for _, im := range imports(s) {
			for _, c := range clientPkgs {
				if im != c && !strings.HasPrefix(im, c+"/") {
					continue
				}
				// main only silences client-go's warning output.
				if s.rel == "cmd/k2stui/main.go" && im == "k8s.io/client-go/rest" {
					continue
				}
				t.Errorf("%s imports %q: only internal/kube may talk to the cluster", s.rel, im)
			}
		}
	}
	// exec/attach/port-forward are not wanted anywhere, kube included.
	for _, s := range sources(t) {
		for _, im := range imports(s) {
			if strings.Contains(im, "remotecommand") || strings.Contains(im, "portforward") || strings.HasPrefix(im, "k8s.io/kubectl") {
				t.Errorf("%s imports %q", s.rel, im)
			}
		}
	}
}

// Inside internal/kube, write.go's MergePatch is the one mutating call. No
// file creates, updates, deletes or applies anything.
func TestSingleWritePath(t *testing.T) {
	writeVerbs := map[string]bool{
		"Patch": true, "Update": true, "UpdateStatus": true, "UpdateScale": true, "Create": true,
		"Delete": true, "DeleteCollection": true, "Apply": true, "ApplyStatus": true, "ApplyScale": true,
		"Evict": true, "EvictV1": true, "EvictV1beta1": true, "Bind": true,
		"Put": true, "Post": true, "Verb": true,
	}
	patchCalls := 0
	for _, s := range sources(t) {
		if !strings.HasPrefix(s.rel, "internal/kube/") || strings.HasPrefix(s.rel, "internal/kube/kubetest/") {
			continue
		}
		calls(s, func(sel string, pos token.Position) {
			if !writeVerbs[sel] {
				return
			}
			if s.rel == "internal/kube/write.go" && sel == "Patch" {
				patchCalls++
				return
			}
			t.Errorf("%s: %s() — internal/kube may only write through MergePatch in write.go", pos, sel)
		})
	}
	if patchCalls != 1 {
		t.Errorf("write.go has %d Patch calls, want exactly 1", patchCalls)
	}

	// The Table listing uses a raw authenticated HTTP client. It may only GET.
	rawRequests := 0
	for _, s := range sources(t) {
		if !strings.HasPrefix(s.rel, "internal/kube/") {
			continue
		}
		ast.Inspect(s.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := s.fset.Position(call.Pos())
			switch sel.Sel.Name {
			case "NewRequestWithContext", "NewRequest":
				rawRequests++
				idx := 0
				if sel.Sel.Name == "NewRequestWithContext" {
					idx = 1
				}
				method, ok := call.Args[idx].(*ast.SelectorExpr)
				if !ok || method.Sel.Name != "MethodGet" {
					t.Errorf("%s: raw HTTP request whose method is not the constant http.MethodGet", pos)
				}
			case "PostForm", "Head":
				t.Errorf("%s: %s() on a raw HTTP client in internal/kube", pos, sel.Sel.Name)
			}
			return true
		})
	}
	if rawRequests != 1 {
		t.Errorf("internal/kube builds raw HTTP requests in %d places, want exactly 1 (the Table listing)", rawRequests)
	}

	// And only the mutate plans call it.
	for _, s := range sources(t) {
		calls(s, func(sel string, pos token.Position) {
			if sel == "MergePatch" && s.rel != "internal/tools/mutate.go" {
				t.Errorf("%s: MergePatch called outside internal/tools/mutate.go", pos)
			}
		})
	}
}

// Plan.Apply — the step that really changes the cluster — is called from one
// place: the agent's gated path, after the gate returned a grant.
func TestApplyIsOnlyCalledFromTheGatedPath(t *testing.T) {
	applies, asks := 0, 0
	for _, s := range sources(t) {
		calls(s, func(sel string, pos token.Position) {
			switch sel {
			case "Apply":
				if s.rel != "internal/agent/mutate.go" {
					t.Errorf("%s: Apply() called outside the gated path", pos)
				}
				applies++
			case "Ask":
				if s.rel != "internal/agent/mutate.go" {
					t.Errorf("%s: Gate.Ask() called outside the gated path", pos)
				}
				asks++
			}
		})
		// approval.Grant can only be minted in its own package.
		if strings.HasPrefix(s.rel, "internal/approval/") {
			continue
		}
		ast.Inspect(s.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Grant" {
				t.Errorf("%s: a Grant is constructed outside internal/approval", s.fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if applies != 1 || asks != 1 {
		t.Errorf("found %d Apply and %d Ask calls, want exactly 1 of each", applies, asks)
	}
}

// The approval gate must not be able to proceed by itself: no timers.
func TestGateHasNoTimers(t *testing.T) {
	for _, s := range sources(t) {
		if !strings.HasPrefix(s.rel, "internal/approval/") {
			continue
		}
		for _, im := range imports(s) {
			if im == "time" {
				t.Errorf("%s imports time: the approval gate must have no timeout or deadline of its own", s.rel)
			}
		}
		calls(s, func(sel string, pos token.Position) {
			switch sel {
			case "After", "AfterFunc", "NewTimer", "NewTicker", "Tick", "WithTimeout", "WithDeadline", "Sleep":
				t.Errorf("%s: %s() in the approval gate", pos, sel)
			}
		})
	}
}

// 7.2: the audit package has no way to rewrite, shorten or remove the log.
// (head.go replaces the small head-anchor sidecar, which is not the log.)
func TestAuditIsAppendOnly(t *testing.T) {
	banned := map[string]bool{
		"Remove": true, "RemoveAll": true, "Truncate": true, "Rename": true, "WriteFile": true,
		"Create": true, "Seek": true, "WriteAt": true, "Chmod": true, "Link": true, "Symlink": true,
	}
	opens := 0
	for _, s := range sources(t) {
		if !strings.HasPrefix(s.rel, "internal/audit/") || s.rel == "internal/audit/head.go" {
			continue
		}
		calls(s, func(sel string, pos token.Position) {
			if banned[sel] {
				t.Errorf("%s: %s() — the audit log is append-only", pos, sel)
			}
			if sel == "OpenFile" {
				opens++
			}
		})
		ast.Inspect(s.file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "O_TRUNC", "O_RDWR":
					t.Errorf("%s: %s — the audit log is opened for appending only", s.fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if opens != 1 {
		t.Errorf("audit opens files for writing in %d places, want exactly 1", opens)
	}
	// Nothing outside the audit package writes to the trail by other means:
	// the only exported write is Log.Append, called from the agent.
	for _, s := range sources(t) {
		if strings.HasPrefix(s.rel, "internal/audit/") {
			continue
		}
		calls(s, func(sel string, pos token.Position) {
			if sel == "Append" && s.rel != "internal/agent/mutate.go" {
				t.Errorf("%s: Append() outside the agent's gated path", pos)
			}
		})
	}
}
