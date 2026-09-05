package clock_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
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
		"Until":     "use Clock.Now and subtract",
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

	// Map each local name to the package it actually refers to, so a local
	// variable called `time` cannot produce a false positive AND an alias like
	// `import t "time"` cannot slip past.
	imported := importedNames(t, rel(root, path), file)

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
		local, ok := sel.X.(*ast.Ident)
		if !ok || local.Obj != nil {
			return true // shadowed by a local declaration, not a package name
		}
		// Resolve through the alias: `import t "time"` must be checked against
		// the rules for "time", not for "t".
		pkg, imported := imported[local.Name]
		if !imported {
			return true
		}
		reason, bad := banned[pkg][sel.Sel.Name]
		if !bad {
			return true
		}
		pos := fset.Position(sel.Pos())
		if allowed[pos.Line] {
			return true
		}
		t.Errorf("%s:%d: %s.%s (%s.%s) is banned outside internal/clock — %s\n"+
			"\t(if this use is genuinely correct, add a `// %s: <reason>` comment on that line)",
			rel(root, path), pos.Line, local.Name, sel.Sel.Name, pkg, sel.Sel.Name, reason, allowMarker)
		return true
	})
}

// importedNames maps the local name of each watched import to its real package
// path.
//
// Keying on the local name alone would let `import t "time"` defeat the entire
// check, because there are no rules registered under "t". A dot import is
// rejected outright: it puts Now and Sleep into the file's own scope, where a
// selector-based walk cannot see them at all.
func importedNames(t *testing.T, path string, file *ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if _, watched := banned[p]; !watched {
			continue
		}
		name := p
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			t.Errorf("%s: dot-importing %q hides its identifiers from this check; import it normally", path, p)
			continue
		}
		if name == "_" {
			continue
		}
		out[name] = p
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

// TestPurityCheckActuallyCatchesViolations is the negative fixture.
//
// A guard that has never been shown to fail is not a guard. This writes source
// that violates the rule in each way that has to be caught — including the
// aliased import that used to slip past, because the check keyed its rules on
// the local name rather than the package path — and asserts the walk reports
// every one of them.
func TestPurityCheckActuallyCatchesViolations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		src     string
		wantHit bool
	}{
		{
			name:    "plain time.Now",
			src:     "package x\nimport \"time\"\nfunc f() { _ = time.Now() }\n",
			wantHit: true,
		},
		{
			name:    "aliased time import",
			src:     "package x\nimport t \"time\"\nfunc f() { _ = t.Now() }\n",
			wantHit: true,
		},
		{
			name:    "context.WithTimeout",
			src:     "package x\nimport (\"context\"\n\"time\")\nfunc f() { _, _ = context.WithTimeout(context.Background(), time.Second) }\n",
			wantHit: true,
		},
		{
			name:    "aliased context import",
			src:     "package x\nimport (c \"context\"\n\"time\")\nfunc f() { _, _ = c.WithDeadline(c.Background(), time.Time{}) }\n",
			wantHit: true,
		},
		{
			// A local variable that happens to be called `time` is not the
			// package, and must not be reported.
			name:    "shadowed identifier is not a violation",
			src:     "package x\ntype s struct{ Now func() int }\nfunc f() { time := s{}; _ = time.Now() }\n",
			wantHit: false,
		},
		{
			name:    "permitted time constructors",
			src:     "package x\nimport \"time\"\nfunc f() { _ = time.Second; _ = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }\n",
			wantHit: false,
		},
		{
			name:    "explicit allow marker",
			src:     "package x\nimport \"time\"\nfunc f() { _ = time.Now() } // clock:allow deliberate\n",
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			// checkFile reports through *testing.T, so run it against a probe
			// and observe whether that probe failed.
			probe := &testing.T{}
			checkFile(probe, dir, path)
			if got := probe.Failed(); got != tc.wantHit {
				t.Errorf("violation detected = %v, want %v\nsource:\n%s", got, tc.wantHit, tc.src)
			}
		})
	}
}
