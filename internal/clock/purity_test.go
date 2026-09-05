package clock_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// banned maps package -> identifiers that must not be referenced anywhere
// under internal/ except in package clock itself.
//
// The time entries are the obvious half: reading wall-clock time inside the
// runtime makes behaviour untestable without sleeping.
//
// The context entries are the half that actually matters. A stray
// context.WithTimeout creates an anonymous deadline that no test can drive and
// that the classifier cannot name — and nothing fails, because the retry it
// produces still looks legitimate. Every deadline in RunMesh must come from
// Clock.WithTimeout (drivable) or clock.WithWriteDeadline (detached from
// cancellation on purpose), so that a step that stopped can always say why.
var banned = map[string]map[string]string{
	"time": {
		"Now":       "use Clock.Now",
		"Sleep":     "use Clock.Sleep, which also honours ctx",
		"After":     "use Clock.After",
		"Tick":      "use Clock.NewTicker",
		"NewTimer":  "use Clock.After",
		"NewTicker": "use Clock.NewTicker",
		"AfterFunc": "use Clock.After and a select",
		"Since":     "use Clock.Since",
	},
	"context": {
		"WithTimeout":       "use Clock.WithTimeout or clock.WithWriteDeadline",
		"WithDeadline":      "use Clock.WithTimeout or clock.WithWriteDeadline",
		"WithTimeoutCause":  "use Clock.WithTimeout or clock.WithWriteDeadline",
		"WithDeadlineCause": "use Clock.WithTimeout or clock.WithWriteDeadline",
	},
}

// allowMarker opts a single line out, and must carry a reason. It exists for
// the handful of places where the real thing is correct — a testing/synctest
// bubble supplies its own fake time, so time.Sleep inside one is precise
// rather than flaky. Grep for it to audit every exception at once.
const allowMarker = "clock:allow"

// TestNoDirectTimeUse is the mechanical half of the cancellation design. It
// walks every Go file under internal/ (production and test alike) except this
// package, and fails the build on any banned reference.
func TestNoDirectTimeUse(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve internal/: %v", err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve self: %v", err)
	}

	var checked int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == self {
				return fs.SkipDir // package clock is the one place time lives
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		checked++
		checkFile(t, root, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if checked == 0 {
		t.Fatal("purity test scanned no files; the walk root is wrong")
	}
	t.Logf("scanned %d files under internal/ (excluding package clock)", checked)
}

func checkFile(t *testing.T, root, path string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Errorf("%s: parse: %v", rel(root, path), err)
		return
	}

	// Only flag a package name the file actually imports under that name, so a
	// local variable called `time` cannot produce a false positive.
	imported := importedNames(file)

	allowed := map[int]bool{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, allowMarker) {
				allowed[fset.Position(c.Pos()).Line] = true
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Obj != nil || !imported[pkg.Name] {
			return true // shadowed by a local declaration, or not that import
		}
		reason, bad := banned[pkg.Name][sel.Sel.Name]
		if !bad {
			return true
		}
		pos := fset.Position(sel.Pos())
		if allowed[pos.Line] {
			return true
		}
		t.Errorf("%s:%d: %s.%s is banned outside internal/clock — %s\n"+
			"\t(if this use is genuinely correct, add a `// %s: <reason>` comment on that line)",
			rel(root, path), pos.Line, pkg.Name, sel.Sel.Name, reason, allowMarker)
		return true
	})
}

// importedNames returns the local names under which the banned packages are
// imported by this file.
func importedNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if _, watched := banned[p]; !watched {
			continue
		}
		name := p
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = true
	}
	return out
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
