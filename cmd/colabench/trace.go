package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// traceResult is what adapters/python/trace.py reports.
type traceResult struct {
	FirstLines []int    `json:"first_lines"`
	RaisedIn   []int    `json:"raised_in"`
	Blocks     int      `json:"blocks"`
	Ran        int      `json:"ran"`
	Errors     []string `json:"errors"`
}

// traceInfo is the trace mapped onto a job's blocks.
type traceInfo struct {
	Snippets int      `json:"snippets"`
	Ran      int      `json:"ran"`
	Raised   []int    `json:"-"` // block positions in the traceback of an error
	Called   []int    `json:"-"` // block positions of functions that ran
	Errors   []string `json:"errors,omitempty"`
	Err      string   `json:"error,omitempty"`
}

const tracePreamble = "from sympy import *\nfrom sympy.abc import a, b, c, k, n, t, x, y, z"

// runTrace runs the report's code against the unpatched checkout, sandboxed:
// no network, and no writes outside a temporary directory.
func runTrace(ctx context.Context, python, tracer, repo string, j *job) traceInfo {
	var ti traceInfo
	tmp, err := os.MkdirTemp("", "colatrace-")
	if err != nil {
		ti.Err = err.Error()
		return ti
	}
	defer os.RemoveAll(tmp)
	tmp, _ = filepath.EvalSymlinks(tmp)
	profile := fmt.Sprintf("(version 1)\n(allow default)\n(deny network*)\n(deny file-write* (subpath %q))\n(allow file-write* (subpath %q))\n",
		os.Getenv("HOME"), tmp)
	if err := os.WriteFile(filepath.Join(tmp, "sb"), []byte(profile), 0o644); err != nil {
		ti.Err = err.Error()
		return ti
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sandbox-exec", "-f", filepath.Join(tmp, "sb"),
		"env", "PYTHONPATH="+repo, "PYTHONDONTWRITEBYTECODE=1", "HOME="+tmp, "MPLBACKEND=Agg",
		python, tracer, filepath.Join(repo, j.file), "--preamble", tracePreamble)
	cmd.Dir = tmp
	cmd.Stdin = strings.NewReader(j.task)
	out, err := cmd.Output()
	var r traceResult
	if jerr := json.Unmarshal(out, &r); jerr != nil {
		ti.Err = fmt.Sprintf("trace: %v %v", err, jerr)
		return ti
	}
	ti.Snippets, ti.Ran, ti.Errors = r.Blocks, r.Ran, r.Errors
	ti.Raised = fnBlocks(j, r.RaisedIn)
	ti.Called = fnBlocks(j, r.FirstLines)
	return ti
}

// fnBlocks maps first lines of functions onto function blocks.
func fnBlocks(j *job, lines []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, l := range lines {
		b := j.doc.BlockOf(l)
		if b < 0 || seen[b] {
			continue
		}
		lab := j.doc.Blocks[b].Label
		if lab == "module" || strings.HasSuffix(lab, " (body)") {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	return out
}

// tiered puts the blocks of each group first, group by group, each group and
// the rest in the order of base.
func tiered(base []string, groups ...[]string) []string {
	placed := map[string]bool{}
	var out []string
	for _, g := range groups {
		in := map[string]bool{}
		for _, id := range g {
			in[id] = true
		}
		for _, id := range base {
			if in[id] && !placed[id] {
				placed[id] = true
				out = append(out, id)
			}
		}
	}
	for _, id := range base {
		if !placed[id] {
			out = append(out, id)
		}
	}
	return out
}

func blockIDs(j *job, pos []int) []string {
	out := make([]string, len(pos))
	for i, p := range pos {
		out[i] = j.doc.Blocks[p].ID
	}
	return out
}
