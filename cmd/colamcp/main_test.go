package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Malko-opensource/costsplit/internal/bench"
)

func newServer(t *testing.T, m string) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	src := "a = 1\nb = 2\nc = 3\n"
	if err := os.WriteFile(filepath.Join(dir, "m.txt"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	*mode, *work = m, dir
	return &server{env: &bench.SWEEnv{Python: "python3"}, files: map[string]*file{}}, dir
}

func TestGateRefusesUnseenSearchThenApplies(t *testing.T) {
	s, dir := newServer(t, "cola")
	// Never read: every line is unseen, so the gate shows it and refuses.
	out, isErr := s.edit(map[string]any{"path": "m.txt", "search": "b = 2", "replace": "b = 20"})
	if !isErr || !strings.Contains(out, "not been shown") || !strings.Contains(out, "2| b = 2") {
		t.Fatalf("gate did not fire: %v %q", isErr, out)
	}
	out, isErr = s.edit(map[string]any{"path": "m.txt", "search": "     2| b = 2", "replace": "b = 20"})
	if isErr {
		t.Fatalf("second edit refused: %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "m.txt"))
	if string(b) != "a = 1\nb = 20\nc = 3\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestFullModeReadsWholeAndEditsWithoutReading(t *testing.T) {
	s, _ := newServer(t, "full")
	out, isErr := s.readFile(context.Background(), map[string]any{"path": "m.txt"})
	if isErr || !strings.Contains(out, "c = 3") || strings.Contains(out, "hidden") {
		t.Fatalf("read: %v %q", isErr, out)
	}
	s2, _ := newServer(t, "full")
	if out, isErr := s2.edit(map[string]any{"path": "m.txt", "search": "a = 1", "replace": "a = 9"}); isErr {
		t.Fatalf("edit: %q", out)
	}
	if _, isErr := s2.readFile(context.Background(), map[string]any{"path": "../x"}); !isErr {
		t.Fatal("path outside the repository was read")
	}
}

func TestShowByLineRange(t *testing.T) {
	s, _ := newServer(t, "cola")
	out, isErr := s.show(map[string]any{"path": "m.txt", "lines": "2-2"})
	if isErr || !strings.Contains(out, "b = 2") {
		t.Fatalf("show: %v %q", isErr, out)
	}
	if _, isErr := s.show(map[string]any{"path": "m.txt", "lines": "2-2"}); !isErr {
		t.Fatal("showing an already shown range should report nothing hidden")
	}
}
