package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Env holds where tools live.
type Env struct {
	BenchRoot string // polyglot-benchmark checkout
	RepoRoot  string // graybox repo (adapters, jars)
	JSDeps    string // directory with node_modules for jest scoring
	JavaHome  string
}

func (e *Env) javaBin(tool string) string { return filepath.Join(e.JavaHome, "bin", tool) }

type Exercise struct {
	Lang         string
	Name         string
	Dir          string
	Solution     []string
	Test         []string
	Editor       []string
	Instructions string
}

// GrayboxTestFile is where the large model writes requirement tests.
var GrayboxTestFile = map[string]string{
	"go":         "graybox_test.go",
	"python":     "test_graybox.py",
	"java":       "src/test/java/GrayboxTest.java",
	"javascript": "graybox.test.mjs",
}

func List(env *Env, lang string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(env.BenchRoot, lang, "exercises/practice"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func Load(env *Env, lang, name string) (*Exercise, error) {
	dir := filepath.Join(env.BenchRoot, lang, "exercises/practice", name)
	var cfg struct {
		Files struct {
			Solution []string `json:"solution"`
			Test     []string `json:"test"`
			Editor   []string `json:"editor"`
		} `json:"files"`
	}
	b, err := os.ReadFile(filepath.Join(dir, ".meta/config.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	var ins strings.Builder
	for _, f := range []string{".docs/introduction.md", ".docs/instructions.md", ".docs/instructions.append.md"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			ins.Write(b)
			ins.WriteString("\n\n")
		}
	}
	return &Exercise{Lang: lang, Name: name, Dir: dir, Solution: cfg.Files.Solution, Test: cfg.Files.Test,
		Editor: cfg.Files.Editor, Instructions: ins.String()}, nil
}

// Prepare copies the exercise into work without .meta (it holds the reference
// solution), .docs, and the official test files (kept hidden for scoring).
func (x *Exercise) Prepare(work string) error {
	hidden := map[string]bool{}
	for _, t := range x.Test {
		hidden[filepath.Clean(t)] = true
	}
	return copyTree(x.Dir, work, func(rel string, d fs.DirEntry) bool {
		top := strings.Split(rel, string(filepath.Separator))[0]
		if top == ".meta" || top == ".docs" || top == "node_modules" {
			return false
		}
		return !hidden[rel]
	})
}

// Files returns the current contents of the given relative paths.
func Files(work string, rels []string) map[string]string {
	out := map[string]string{}
	for _, r := range rels {
		if b, err := os.ReadFile(filepath.Join(work, r)); err == nil {
			out[r] = string(b)
		}
	}
	return out
}

// Score copies work, removes graybox-only files, restores the official tests
// (un-skipping them as the Aider benchmark does) and runs them.
func (x *Exercise) Score(ctx context.Context, env *Env, work string) (bool, string) {
	dir, err := os.MkdirTemp("", "polybench-score-")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(dir)
	skip := map[string]bool{".graybox": true, "node_modules": true, "build": true, "__pycache__": true}
	gb := filepath.Clean(GrayboxTestFile[x.Lang])
	if err := copyTree(work, dir, func(rel string, d fs.DirEntry) bool {
		return !skip[strings.Split(rel, string(filepath.Separator))[0]] && rel != gb
	}); err != nil {
		return false, err.Error()
	}
	for _, t := range x.Test {
		b, err := os.ReadFile(filepath.Join(x.Dir, t))
		if err != nil {
			return false, err.Error()
		}
		s := string(b)
		switch x.Lang {
		case "java":
			s = strings.ReplaceAll(s, "@Disabled", "// @Disabled")
		case "javascript":
			s = strings.ReplaceAll(s, "xtest(", "test(")
			s = strings.ReplaceAll(s, "xit(", "it(")
		}
		dst := filepath.Join(dir, t)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, []byte(s), 0o644); err != nil {
			return false, err.Error()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	switch x.Lang {
	case "go":
		return run(ctx, dir, nil, "go", "test", "-count=1", "./...")
	case "python":
		return run(ctx, dir, nil, "python3", "-m", "unittest", "discover", "-p", "*_test.py")
	case "javascript":
		if err := os.Symlink(filepath.Join(env.JSDeps, "node_modules"), filepath.Join(dir, "node_modules")); err != nil {
			return false, err.Error()
		}
		args := append([]string{"--ci", "--silent"}, x.Test...)
		return run(ctx, dir, nil, filepath.Join(env.JSDeps, "node_modules/.bin/jest"), args...)
	case "java":
		return x.scoreJava(ctx, env, dir)
	}
	return false, "unknown language " + x.Lang
}

func (x *Exercise) scoreJava(ctx context.Context, env *Env, dir string) (bool, string) {
	cp := JavaClasspath(env)
	mainOut, testOut := filepath.Join(dir, "out/main"), filepath.Join(dir, "out/test")
	mains, _ := filepath.Glob(filepath.Join(dir, "src/main/java/*.java"))
	mains = append(mains, globDeep(filepath.Join(dir, "src/main/java"))...)
	mains = uniq(mains)
	if ok, out := run(ctx, dir, nil, env.javaBin("javac"), append([]string{"-nowarn", "-d", mainOut, "-cp", cp}, mains...)...); !ok {
		return false, out
	}
	tests := globDeep(filepath.Join(dir, "src/test/java"))
	if ok, out := run(ctx, dir, nil, env.javaBin("javac"), append([]string{"-nowarn", "-d", testOut, "-cp", mainOut + ":" + cp}, tests...)...); !ok {
		return false, out
	}
	return run(ctx, dir, nil, env.javaBin("java"), "-jar", junitJar(env), "execute",
		"--class-path", mainOut+":"+testOut+":"+cp, "--scan-class-path", "--disable-banner", "--details=summary", "--fail-if-no-tests")
}

// JavaClasspath is every jar in adapters/jvm/lib except the JUnit console:
// AssertJ (official tests) and the few libraries some exercises import.
func JavaClasspath(env *Env) string {
	jars, _ := filepath.Glob(filepath.Join(env.RepoRoot, "adapters/jvm/lib/*.jar"))
	var keep []string
	for _, j := range jars {
		if !strings.Contains(filepath.Base(j), "junit-platform-console") {
			keep = append(keep, j)
		}
	}
	keep = append(keep, junitJar(env))
	return strings.Join(keep, ":")
}

func junitJar(env *Env) string {
	m, _ := filepath.Glob(filepath.Join(env.RepoRoot, "adapters/jvm/lib/junit-platform-console-standalone-*.jar"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

func globDeep(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".java") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) (bool, string) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return false, "timeout\n" + tail(s, 3000)
	}
	if err != nil {
		return false, tail(s, 3000)
	}
	return true, tail(s, 1000)
}

func copyTree(src, dst string, keep func(rel string, d fs.DirEntry) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if !keep(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

func fileList(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "=== %s ===\n%s\n", k, m[k])
	}
	return b.String()
}
