package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocate(t *testing.T) {
	base, work := filepath.Join(t.TempDir(), "base"), filepath.Join(t.TempDir(), "work")
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line"
	}
	write := func(dir, name string, edit map[int]string) {
		l := append([]string{}, lines...)
		for i, v := range edit {
			l[i-1] = v
		}
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(l, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gold := "diff --git a/a.c b/a.c\n--- a/a.c\n+++ b/a.c\n@@ -39,3 +39,3 @@ f\n line\n-line\n+fixed\n line\n"
	s := &SWE{gold: gold, LocateOnly: true}
	for _, c := range []struct {
		name string
		file string
		at   int
		hit  bool
	}{{"same line", "a.c", 40, true}, {"near", "a.c", 50, true}, {"far", "a.c", 90, false}, {"other file", "b.c", 40, false}} {
		os.RemoveAll(base)
		os.RemoveAll(work)
		write(base, "a.c", nil)
		write(base, "b.c", nil)
		write(work, "a.c", nil)
		write(work, "b.c", nil)
		write(work, c.file, map[int]string{c.at: "changed"})
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("no git")
		}
		loc := s.Locate(&SWEEnv{RepoDir: base}, work)
		if loc.Hit() != c.hit || loc.GoldHunks != 1 {
			t.Errorf("%s: %s", c.name, loc)
		}
	}
}
