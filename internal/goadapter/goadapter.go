// Package goadapter is the Go language adapter. Symbols look like
// "order.(*Service).Apply", "order.(Service).Name" or "order.NewService".
package goadapter

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/local/graybox/internal/adapter"
	"golang.org/x/tools/imports"
)

type Adapter struct{ Dir string }

func New(dir string) *Adapter { return &Adapter{Dir: dir} }

func (a *Adapter) Lang() string { return "go" }

type symRef struct{ pkg, recv, name string }

func parseSymbol(s string) (symRef, error) {
	dot := strings.Index(s, ".")
	if dot <= 0 {
		return symRef{}, fmt.Errorf("bad Go symbol %q: want pkg.Func or pkg.(*Type).Method", s)
	}
	ref := symRef{pkg: s[:dot]}
	rest := s[dot+1:]
	if strings.HasPrefix(rest, "(") {
		end := strings.Index(rest, ").")
		if end < 0 {
			return symRef{}, fmt.Errorf("bad Go symbol %q", s)
		}
		ref.recv = strings.TrimPrefix(rest[1:end], "*")
		ref.name = rest[end+2:]
	} else {
		ref.name = rest
	}
	if ref.name == "" {
		return symRef{}, fmt.Errorf("bad Go symbol %q", s)
	}
	return ref, nil
}

type found struct {
	path string
	fset *token.FileSet
	file *ast.File
	decl *ast.FuncDecl
	src  []byte
}

func (a *Adapter) goFiles() ([]string, error) {
	var out []string
	err := filepath.WalkDir(a.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != a.Dir && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func (a *Adapter) find(symbol string) (*found, error) {
	ref, err := parseSymbol(symbol)
	if err != nil {
		return nil, err
	}
	files, err := a.goFiles()
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, parser.ParseComments)
		if err != nil {
			continue
		}
		if f.Name.Name != ref.pkg && f.Name.Name != ref.pkg+"_test" {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != ref.name || recvName(fd) != ref.recv {
				continue
			}
			return &found{path: p, fset: fset, file: f, decl: fd, src: src}, nil
		}
	}
	return nil, fmt.Errorf("symbol %s not found", symbol)
}

func signature(fset *token.FileSet, fd *ast.FuncDecl) string {
	cp := *fd
	cp.Body = nil
	cp.Doc = nil
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, &cp)
	return normalize(b.String())
}

// isStub: body is exactly panic(<string literal>) or t.Skip(...).
func isStub(fd *ast.FuncDecl) bool {
	if fd.Body == nil || len(fd.Body.List) != 1 {
		return false
	}
	es, ok := fd.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		// panic("not implemented"), or any string literal such as
		// Exercism's panic("Please implement the Solve function").
		if fn.Name == "panic" && len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				_, err := strconv.Unquote(lit.Value)
				return err == nil
			}
		}
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Skip"
	}
	return false
}

func (a *Adapter) Resolve(symbol string) (adapter.Symbol, error) {
	f, err := a.find(symbol)
	if err != nil {
		return adapter.Symbol{}, err
	}
	rel, _ := filepath.Rel(a.Dir, f.path)
	return adapter.Symbol{Name: symbol, File: rel, IsStub: isStub(f.decl), Signature: signature(f.fset, f.decl)}, nil
}

func (a *Adapter) Apply(symbol, body string) error {
	if _, err := parser.ParseFile(token.NewFileSet(), "body.go", "package p\nfunc _() {\n"+body+"\n}\n", 0); err != nil {
		return fmt.Errorf("body does not parse: %w", err)
	}
	f, err := a.find(symbol)
	if err != nil {
		return err
	}
	if f.decl.Body == nil {
		return fmt.Errorf("%s has no body", symbol)
	}
	lb := f.fset.Position(f.decl.Body.Lbrace).Offset
	rb := f.fset.Position(f.decl.Body.Rbrace).Offset
	var out bytes.Buffer
	out.Write(f.src[:lb+1])
	out.WriteString("\n" + strings.Trim(body, "\n") + "\n")
	out.Write(f.src[rb:])
	// Bodies cannot add imports themselves and Go rejects unused ones, so
	// imports are fixed the way goimports does (Q-014 for Go).
	formatted, err := imports.Process(f.path, out.Bytes(), &imports.Options{Comments: true, TabIndent: true, TabWidth: 8, FormatOnly: false})
	if err != nil {
		return fmt.Errorf("result does not format: %w", err)
	}
	return os.WriteFile(f.path, formatted, 0o644)
}

func (a *Adapter) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = a.Dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *Adapter) Checks() []adapter.Check {
	var cs []adapter.Check
	if out, err := a.run("gofmt", "-l", "."); err != nil || out != "" {
		cs = append(cs, adapter.Check{Name: "gofmt", Status: adapter.CheckFail, Detail: "not formatted: " + out})
	} else {
		cs = append(cs, adapter.Check{Name: "gofmt", Status: adapter.CheckPass})
	}
	build := a.cmdCheck("build", "go", "build", "./...")
	cs = append(cs, build)
	if build.Status == adapter.CheckFail {
		// Later checks would only repeat the compile error.
		return append(cs,
			adapter.Check{Name: "vet", Status: adapter.CheckSkipped, Detail: "build failed"},
			adapter.Check{Name: "lint", Status: adapter.CheckSkipped, Detail: "build failed"},
			adapter.Check{Name: "test", Status: adapter.CheckSkipped, Detail: "build failed"})
	}
	cs = append(cs, a.cmdCheck("vet", "go", "vet", "./..."))
	if _, err := exec.LookPath("golangci-lint"); err == nil {
		cs = append(cs, a.cmdCheck("lint", "golangci-lint", "run"))
	} else {
		cs = append(cs, adapter.Check{Name: "lint", Status: adapter.CheckSkipped, Detail: "golangci-lint not installed"})
	}
	cs = append(cs, a.cmdCheck("test", "go", "test", "-count=1", "./..."))
	return cs
}

func (a *Adapter) cmdCheck(name string, cmd string, args ...string) adapter.Check {
	out, err := a.run(cmd, args...)
	if err != nil {
		return adapter.Check{Name: name, Status: adapter.CheckFail, Detail: tail(out, 2000)}
	}
	return adapter.Check{Name: name, Status: adapter.CheckPass}
}

var testLine = regexp.MustCompile(`^(Test|Example|Fuzz|Benchmark)\w*$`)

func (a *Adapter) ListTests() ([]string, error) {
	out, err := a.run("go", "test", "-list", ".*", "./...")
	if err != nil {
		return nil, fmt.Errorf("go test -list: %v: %s", err, tail(out, 500))
	}
	var tests []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if testLine.MatchString(l) {
			tests = append(tests, l)
		}
	}
	sort.Strings(tests)
	return tests, nil
}

// APISnapshot lists exported funcs/methods (signature only), exported types
// (full spec) and exported consts/vars including their values, so a change to
// the state table is caught too.
func (a *Adapter) APISnapshot() ([]string, error) {
	files, err := a.goFiles()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		pkg := f.Name.Name
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				r := recvName(d)
				if !d.Name.IsExported() || (r != "" && !ast.IsExported(r)) {
					continue
				}
				out = append(out, pkg+": "+signature(fset, d))
			case *ast.GenDecl:
				for _, s := range d.Specs {
					var name string
					switch s := s.(type) {
					case *ast.TypeSpec:
						name = s.Name.Name
					case *ast.ValueSpec:
						name = s.Names[0].Name
					}
					if !ast.IsExported(name) {
						continue
					}
					var b bytes.Buffer
					_ = printer.Fprint(&b, fset, s)
					out = append(out, pkg+": "+d.Tok.String()+" "+normalize(b.String()))
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Stubs lists stub functions as pkg.Func, pkg.(*T).M or pkg.(T).M.
func (a *Adapter) Stubs() ([]string, error) {
	files, err := a.goFiles()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !isStub(fd) {
				continue
			}
			name := f.Name.Name + "." + fd.Name.Name
			if r := recvName(fd); r != "" {
				star := ""
				if _, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
					star = "*"
				}
				name = fmt.Sprintf("%s.(%s%s).%s", f.Name.Name, star, r, fd.Name.Name)
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Go is statically typed; nothing extra is required before locking.
func (a *Adapter) Preconditions() []adapter.Check { return nil }

var spaces = regexp.MustCompile(`\s+`)

func normalize(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
