// Package lint holds repository-wide rules enforced as ordinary Go tests, so
// they run on every platform via `go test ./...` and need no external tooling.
package lint_test

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

// bannedTimeFuncs may only be called from internal/clock. Every other package
// takes a Clock, which is what makes the timer scheduler, the escalation engine
// and the schedule resolver testable without sleeping. See docs/DECISIONS.md D5.
var bannedTimeFuncs = map[string]bool{
	"Now":       true,
	"Sleep":     true,
	"After":     true,
	"AfterFunc": true,
	"Tick":      true,
	"NewTimer":  true,
	"NewTicker": true,
}

// skipDirs are not part of the module's own source.
var skipDirs = map[string]bool{
	".git": true, ".github": true, ".toolchain": true, ".gopath": true,
	".gocache": true, ".tmp": true, "vendor": true, "docs": true, "examples": true,
	"scripts": true,
}

const (
	// allowLine permits one call, on the same line or the line above.
	allowLine = "//kerberon:allow-clock"
	// allowFile exempts a whole file. Reserved for code that genuinely cannot
	// use a fake clock, such as the chaos harness, which spans real process
	// restarts.
	allowFile = "//kerberon:allow-clock-file"
)

// TestOnlyClockPackageTouchesTime replaces the CI grep that previously enforced
// this. A grep cannot tell code from prose — it flagged a doc comment in
// internal/timer that merely mentions time.AfterFunc — and it cannot support a
// reviewed exemption. Parsing gets both right.
func TestOnlyClockPackageTouchesTime(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	clockDir := filepath.Join(root, "internal", "clock")
	var violations []string
	var scanned int

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// internal/clock is the one package permitted to read the clock.
			if path == clockDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		found, err := checkFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, v := range found {
			rel, _ := filepath.Rel(root, v.file)
			violations = append(violations,
				filepath.ToSlash(rel)+":"+itoa(v.line)+"  time."+v.fn)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if scanned == 0 {
		t.Fatal("scanned no Go files; the walk is misconfigured and this test proves nothing")
	}

	if len(violations) > 0 {
		t.Errorf("direct time access is only permitted in internal/clock.\n"+
			"Take a clock.Clock instead, or mark a reviewed exception with %s.\n\n  %s",
			allowLine, strings.Join(violations, "\n  "))
	}
}

// scratchDir allocates a temporary directory inside the project rather than
// calling t.TempDir(), which would write to the system temp directory and
// breach CLAUDE.md R1.
func scratchDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "lint-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type violation struct {
	file string
	line int
	fn   string
}

func checkFile(path string) ([]violation, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(src)

	// A file-level exemption covers everything in it.
	if strings.Contains(text, allowFile) {
		return nil, nil
	}

	fset := token.NewFileSet()
	// Comments are parsed but never inspected, so prose mentioning
	// time.AfterFunc cannot trip the rule.
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// "time" may be aliased or dot-imported; resolve what it is called here.
	timeNames := timeImportNames(file)
	if len(timeNames) == 0 {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	var out []violation

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || !timeNames[ident.Name] {
			return true
		}
		if !bannedTimeFuncs[sel.Sel.Name] {
			return true
		}

		pos := fset.Position(sel.Pos())
		if allowedAt(lines, pos.Line) {
			return true
		}
		out = append(out, violation{file: path, line: pos.Line, fn: sel.Sel.Name})
		return true
	})
	return out, nil
}

// timeImportNames returns the local names under which the time package is
// imported, so an alias cannot evade the rule.
func timeImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"time"` {
			continue
		}
		switch {
		case imp.Name == nil:
			names["time"] = true
		case imp.Name.Name == "_", imp.Name.Name == ".":
			// Blank imports cannot call anything. A dot import would need
			// bare-identifier analysis; it is not used anywhere and would be
			// rejected in review.
		default:
			names[imp.Name.Name] = true
		}
	}
	return names
}

// allowedAt reports whether an exemption marker sits on the given line or the
// line above it.
func allowedAt(lines []string, line int) bool {
	idx := line - 1
	if idx >= 0 && idx < len(lines) && strings.Contains(lines[idx], allowLine) {
		return true
	}
	if idx-1 >= 0 && idx-1 < len(lines) && strings.Contains(lines[idx-1], allowLine) {
		return true
	}
	return false
}

// itoa avoids importing strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// The rule is worthless if the checker cannot see a violation, so prove it can.
func TestCheckerDetectsAViolation(t *testing.T) {
	dir := scratchDir(t)
	path := filepath.Join(dir, "bad.go")
	src := `package bad

import "time"

func f() time.Time { return time.Now() }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	found, err := checkFile(path)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(found) != 1 || found[0].fn != "Now" {
		t.Fatalf("checker found %v, want one time.Now violation", found)
	}
}

func TestCheckerIgnoresComments(t *testing.T) {
	dir := scratchDir(t)
	path := filepath.Join(dir, "commented.go")
	src := `package commented

import "time"

// This mentions time.Now and time.AfterFunc in prose only.
func f() time.Duration { /* time.Sleep */ return time.Second }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	found, err := checkFile(path)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("prose tripped the rule: %v", found)
	}
}

func TestCheckerHonoursExemptions(t *testing.T) {
	dir := scratchDir(t)
	for _, c := range []struct {
		name string
		src  string
	}{
		{"same line", `package a

import "time"

func f() time.Time { return time.Now() } //kerberon:allow-clock
`},
		{"line above", `package a

import "time"

func f() time.Time {
	//kerberon:allow-clock
	return time.Now()
}
`},
		{"whole file", `//kerberon:allow-clock-file
package a

import "time"

func f() time.Time { return time.Now() }
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".go")
			if err := os.WriteFile(path, []byte(c.src), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			found, err := checkFile(path)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(found) != 0 {
				t.Fatalf("exemption ignored: %v", found)
			}
		})
	}
}

// An alias must not provide an escape hatch.
func TestCheckerCatchesAliasedImports(t *testing.T) {
	dir := scratchDir(t)
	path := filepath.Join(dir, "aliased.go")
	src := `package aliased

import clockish "time"

func f() clockish.Time { return clockish.Now() }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	found, err := checkFile(path)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("aliased import evaded the rule: %v", found)
	}
}
