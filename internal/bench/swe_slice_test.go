package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCascadeArm(t *testing.T) {
	cases := map[string]CascadeOpt{
		"c5":   {SmallTries: 5},
		"c5p":  {SmallTries: 5, Plan: true},
		"c2t":  {SmallTries: 2, ShowTests: true},
		"c2h2": {SmallTries: 2, HintTries: 2},
	}
	for arm, want := range cases {
		got, ok := ParseCascadeArm(arm)
		if !ok || got != want {
			t.Errorf("%s: got %+v ok=%v, want %+v", arm, got, ok, want)
		}
	}
	for _, arm := range []string{"c0", "small", "c5x"} {
		if _, ok := ParseCascadeArm(arm); ok {
			t.Errorf("%s: accepted", arm)
		}
	}
}

func TestParseSWEArm(t *testing.T) {
	role, opt, ok := ParseSWEArm("large-hleanarch2")
	want := SWEOpt{Slice: "small", Lean: true, Arch: true, More: true}
	if !ok || role != "large" || opt != want {
		t.Errorf("large-hleanarch2: got %q %+v ok=%v", role, opt, ok)
	}
	if _, opt, ok := ParseSWEArm("small-locate"); !ok || !opt.LocateOnly {
		t.Errorf("small-locate: got %+v ok=%v", opt, ok)
	}
	for _, arm := range []string{"medium-ctx", "small", "large-nope"} {
		if _, _, ok := ParseSWEArm(arm); ok {
			t.Errorf("%s: accepted", arm)
		}
	}
}

func TestGoldLines(t *testing.T) {
	patch := `diff --git a/m.py b/m.py
--- a/m.py
+++ b/m.py
@@ -10,5 +10,5 @@ def f():
     a = 1
-    b = 2
+    b = 3
     c = 3
+    d = 4
     return a
`
	// Line 11 is replaced; the insertion after line 12 counts as line 12.
	if got, want := goldLines(patch), []int{11, 12}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCleanNames(t *testing.T) {
	raw := []string{"foo (in another file, not this one)", "A.b - best guess", "", "foo", "`C.d`", "e"}
	if got, want := cleanNames(raw, 3), []string{"foo", "A.b", "C.d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCoversAllowsAdjacentLines(t *testing.T) {
	sl := &fileSlice{Blocks: []sliceBlock{{Start: 10, End: 20}}}
	if !sl.covers([]int{10, 22}) {
		t.Error("a line two below the block should count")
	}
	if sl.covers([]int{23}) {
		t.Error("a line three below the block should not count")
	}
}

const sliceSample = `import os


class Printer:
    """Prints things."""

    def print_add(self, e):
        return self.join(e, "+")

    def print_mul(self, e):
        return self.join(e, "*")

    def join(self, e, op):
        return op.join(e)


def helper(x):
    return x
`

// The slicer is a Python script; these tests run it as the harness does.
func TestSlicer(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	wd, _ := os.Getwd()
	env := &SWEEnv{Python: py, RepoRoot: filepath.Clean(filepath.Join(wd, "../.."))}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "m.py"), []byte(sliceSample), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("existing method with its callee", func(t *testing.T) {
		sl, err := buildSlice(env, work, "m.py", nil, []string{"Printer.print_add"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(sl.Blocks) != 1 || sl.Blocks[0].Label != "Printer.print_add" {
			t.Fatalf("blocks: %+v", sl.Blocks)
		}
		if len(sl.Related) != 1 || sl.Related[0].Label != "Printer.join" {
			t.Errorf("related: %+v", sl.Related)
		}
		if strings.Contains(sl.Outline, "def helper") {
			t.Errorf("lean outline lists other definitions:\n%s", sl.Outline)
		}
	})

	t.Run("method that does not exist yet", func(t *testing.T) {
		sl, err := buildSlice(env, work, "m.py", nil, []string{"Printer.print_sub"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sl.Added, []string{"Printer.print_sub"}) || len(sl.Missing) != 0 {
			t.Fatalf("added %v missing %v", sl.Added, sl.Missing)
		}
		if len(sl.Blocks) == 0 || sl.lines() >= sl.FileLines+10 {
			t.Errorf("want a slice around the class, got %d blocks", len(sl.Blocks))
		}
		if !strings.Contains(sl.Outline, "def print_mul") {
			t.Errorf("outline should list the class's members:\n%s", sl.Outline)
		}
	})

	t.Run("strict guesses nothing", func(t *testing.T) {
		sl, err := buildSlice(env, work, "m.py", nil, []string{"Printer.print_sub", "nothing_like_it"}, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(sl.Blocks) != 0 || len(sl.Missing) != 2 {
			t.Errorf("blocks %d missing %v", len(sl.Blocks), sl.Missing)
		}
	})

	t.Run("choices for the localizer", func(t *testing.T) {
		got := functionNames(env, work, "m.py")
		want := []string{"Printer.print_add", "Printer.print_mul", "Printer.join", "helper"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("lines pick the enclosing function", func(t *testing.T) {
		sl, err := buildSlice(env, work, "m.py", []int{11}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(sl.Blocks) != 1 || sl.Blocks[0].Label != "Printer.print_mul" || !sl.covers([]int{11}) {
			t.Errorf("blocks: %+v", sl.Blocks)
		}
	})
}
