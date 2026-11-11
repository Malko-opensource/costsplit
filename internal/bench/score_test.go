package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestScoreReference checks the scorer on every exercise: the reference
// solution from .meta must pass. Starting code that already passes is
// logged: those are refactoring exercises.
// Set POLYBENCH_ROOT to the polyglot-benchmark checkout to run it.
func TestScoreReference(t *testing.T) {
	root := os.Getenv("POLYBENCH_ROOT")
	if root == "" {
		t.Skip("POLYBENCH_ROOT not set")
	}
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	env := &Env{BenchRoot: root, RepoRoot: filepath.Clean(filepath.Join(wd, "../..")), JSDeps: filepath.Join(home, "bench-data/js-deps"), JavaHome: "/opt/homebrew/opt/openjdk"}
	langs := strings.Split(envOr("POLYBENCH_LANGS", "go,java,python,javascript"), ",")
	ctx := context.Background()
	var mu sync.Mutex
	var bad []string
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, lang := range langs {
		names, err := List(env, lang)
		if err != nil {
			t.Fatal(err)
		}
		for _, nm := range names {
			wg.Add(1)
			sem <- struct{}{}
			go func(lang, nm string) {
				defer wg.Done()
				defer func() { <-sem }()
				x, err := Load(env, lang, nm)
				if err != nil {
					mu.Lock()
					bad = append(bad, lang+"/"+nm+": load: "+err.Error())
					mu.Unlock()
					return
				}
				work := t.TempDir()
				if err := x.Prepare(work); err != nil {
					t.Error(err)
					return
				}
				stubOK, _ := x.Score(ctx, env, work)
				if err := copyReference(x, work); err != nil {
					mu.Lock()
					bad = append(bad, lang+"/"+nm+": reference: "+err.Error())
					mu.Unlock()
					return
				}
				refOK, out := x.Score(ctx, env, work)
				mu.Lock()
				defer mu.Unlock()
				if !refOK {
					bad = append(bad, lang+"/"+nm+": reference fails\n"+tail(out, 400))
				} else if stubOK {
					// Refactoring exercises ship working code; that is expected.
					t.Logf("%s/%s: starting code already passes (refactoring exercise)", lang, nm)
				}
			}(lang, nm)
		}
	}
	wg.Wait()
	for _, b := range bad {
		t.Log(b)
	}
	if len(bad) > 0 {
		t.Errorf("%d exercises not scoreable", len(bad))
	}
}

// copyReference writes .meta's example over the solution files.
func copyReference(x *Exercise, work string) error {
	var cfg struct {
		Files struct {
			Example []string `json:"example"`
		} `json:"files"`
	}
	b, err := os.ReadFile(filepath.Join(x.Dir, ".meta/config.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if x.Lang == "java" {
		// Java references may span several classes: copy them all.
		refs, _ := filepath.Glob(filepath.Join(x.Dir, ".meta/src/reference/java/*.java"))
		for _, r := range refs {
			if err := cp(r, filepath.Join(work, "src/main/java", filepath.Base(r))); err != nil {
				return err
			}
		}
		return nil
	}
	if len(cfg.Files.Example) != len(x.Solution) {
		// Match by base name when there are several files.
		for _, ex := range cfg.Files.Example {
			for _, s := range x.Solution {
				if filepath.Base(ex) == filepath.Base(s) {
					if err := cp(filepath.Join(x.Dir, ex), filepath.Join(work, s)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for i, ex := range cfg.Files.Example {
		if err := cp(filepath.Join(x.Dir, ex), filepath.Join(work, x.Solution[i])); err != nil {
			return err
		}
	}
	return nil
}

func cp(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, b, 0o644)
}

func boolS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
