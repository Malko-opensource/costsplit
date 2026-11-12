// Command colamcp is an MCP server (stdio) that gives an agent the file tools
// of one benchmark arm: read_file, show and edit over a working copy.
//
//	-mode full  read_file returns the whole file with line numbers
//	-mode cola  read_file returns a diet-cola view: Haiku names the blocks the
//	            task needs, the view keeps them plus the best of the rest up
//	            to a budget, small files pass whole, and each hidden run is a
//	            marker naming its functions. show reveals hidden functions.
//	            edit refuses search text that copies lines the agent was not
//	            shown, and shows them instead (the quote gate).
//
// Every ranking call and tool call is appended to -ledger as one JSON line,
// so the harness can bill the small model and count what the agent read.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Malko-opensource/costsplit/internal/bench"
	"github.com/Malko-opensource/costsplit/internal/cola"
)

var (
	mode     = flag.String("mode", "cola", "full or cola")
	work     = flag.String("work", ".", "working copy the tools act on")
	python   = flag.String("python", "python3", "interpreter for slice.py and py_compile")
	root     = flag.String("root", ".", "costsplit repository (adapters)")
	taskFile = flag.String("task", "", "file holding the task (the issue text)")
	ledger   = flag.String("ledger", "", "append usage and tool events here")
	small    = flag.String("small", "haiku", "model that ranks blocks")
	budget   = flag.Float64("budget", 0.2, "share of a cut file's tokens the view keeps beyond the picks")
	whole    = flag.Int("whole", 4000, "files under this many estimated tokens are shown whole")
)

type file struct {
	doc    *cola.Doc
	kept   map[string]bool
	focus  map[string]bool
	ranked bool
}

type server struct {
	env   *bench.SWEEnv
	task  string
	files map[string]*file
	mu    sync.Mutex
	log   *os.File
}

func (s *server) record(ev map[string]any) {
	if s.log == nil {
		return
	}
	b, _ := json.Marshal(ev)
	s.mu.Lock()
	s.log.Write(append(b, '\n'))
	s.mu.Unlock()
}

func (s *server) rel(p string) (string, error) {
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(*work, p)
		if err != nil {
			return "", err
		}
		p = r
	}
	p = filepath.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%s is outside the repository", p)
	}
	return p, nil
}

func (s *server) load(path string) (*file, error) {
	if f, ok := s.files[path]; ok {
		return f, nil
	}
	d, err := bench.ColaDoc(s.env, *work, path)
	if err != nil {
		return nil, err
	}
	f := &file{doc: d, kept: map[string]bool{}, focus: map[string]bool{}}
	s.files[path] = f
	return f, nil
}

func keepAll(f *file) {
	for _, b := range f.doc.Blocks {
		f.kept[b.ID] = true
	}
}

func (s *server) rank(ctx context.Context, path string, f *file, focus string) {
	total := f.doc.TotalTokens()
	if *mode == "full" || total < *whole {
		keepAll(f)
		return
	}
	if f.ranked && (focus == "" || f.focus[focus]) {
		return
	}
	f.focus[focus] = true
	task := s.task
	if focus != "" {
		task += "\n\n# What the reader is looking for in this file now\n" + focus
	}
	c := func(ctx context.Context, system, prompt string, schema map[string]any) (json.RawMessage, float64, error) {
		raw, u, err := (&bench.ClaudeCLI{}).Complete(ctx, *small, system, prompt, schema)
		s.record(map[string]any{"event": "rank", "path": path, "cost_usd": u.CostUSD, "input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "error": errString(err)})
		return raw, u.CostUSD, err
	}
	m, err := cola.Model(ctx, c, f.doc, task, "", cola.ModelOpts{Max: 8, MaxPrompt: 70000, Samples: 1})
	if err != nil {
		keepAll(f)
		return
	}
	for id := range cola.Keep(f.doc, m.Ranking, int(*budget*float64(total))) {
		f.kept[id] = true
	}
	for _, id := range m.Picks {
		f.kept[id] = true
	}
	f.ranked = true
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// numbered renders blocks whole, with line numbers, for show and the gate.
func numbered(d *cola.Doc, blocks []int) string {
	sort.Ints(blocks)
	var b strings.Builder
	for _, i := range blocks {
		bl := d.Blocks[i]
		for l := bl.Start; l <= bl.End; l++ {
			fmt.Fprintf(&b, "%6d| %s\n", l, d.Lines[l-1])
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *server) readFile(ctx context.Context, a map[string]any) (string, bool) {
	path, err := s.rel(str(a["path"]))
	if err != nil {
		return err.Error(), true
	}
	f, err := s.load(path)
	if err != nil {
		return err.Error(), true
	}
	s.rank(ctx, path, f, strings.TrimSpace(str(a["focus"])))
	v := cola.RenderNames(f.doc, f.kept, 0)
	s.record(map[string]any{"event": "read", "path": path, "file_tokens": f.doc.TotalTokens(), "view_tokens": cola.EstTokens(v.Text), "hidden_runs": len(v.Hidden)})
	return v.Text, false
}

func (s *server) show(a map[string]any) (string, bool) {
	path, err := s.rel(str(a["path"]))
	if err != nil {
		return err.Error(), true
	}
	f, err := s.load(path)
	if err != nil {
		return err.Error(), true
	}
	want := map[string]bool{}
	for _, x := range list(a["functions"]) {
		want[x] = true
	}
	lo, hi := 0, -1
	if r := str(a["lines"]); r != "" {
		x, y, _ := strings.Cut(r, "-")
		lo, _ = strconv.Atoi(strings.TrimSpace(x))
		hi, _ = strconv.Atoi(strings.TrimSpace(y))
		if hi == 0 {
			hi = lo
		}
	}
	var got []int
	for i, b := range f.doc.Blocks {
		if f.kept[b.ID] {
			continue
		}
		if want[b.Label] || (hi >= lo && b.Start <= hi && b.End >= lo) {
			f.kept[b.ID] = true
			got = append(got, i)
		}
	}
	s.record(map[string]any{"event": "show", "path": path, "blocks": len(got)})
	if len(got) == 0 {
		return "Nothing hidden matched. Hidden runs are the \"… [eN: lines A-B hidden: names]\" lines in read_file's output; pass those names or a line range.", true
	}
	return numbered(f.doc, got), false
}

func (s *server) edit(a map[string]any) (string, bool) {
	path, err := s.rel(str(a["path"]))
	if err != nil {
		return err.Error(), true
	}
	search, replace := bench.StripLineNumbers(str(a["search"])), bench.StripLineNumbers(str(a["replace"]))
	if *mode == "cola" {
		f, err := s.load(path)
		if err != nil {
			return err.Error(), true
		}
		seen := map[int]bool{}
		var hid []int
		for _, l := range strings.Split(search, "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			q := cola.CheckQuote(f.doc, f.kept, l)
			if q.InText && !q.Shown {
				for _, b := range q.Hidden {
					if !seen[b] {
						seen[b] = true
						hid = append(hid, b)
					}
				}
			}
		}
		if len(hid) > 0 {
			for _, b := range hid {
				f.kept[f.doc.Blocks[b].ID] = true
			}
			s.record(map[string]any{"event": "gate", "path": path, "blocks": len(hid)})
			return "Not applied: the search text copies lines of " + path + " you have not been shown. They are below; copy search text only from code you have seen, then edit again.\n\n" + numbered(f.doc, hid), true
		}
	}
	raw, _ := json.Marshal(map[string]any{"edits": []map[string]string{{"path": path, "search": search, "replace": replace}}})
	if err := bench.ApplyEdits(s.env, *work, raw); err != nil {
		s.record(map[string]any{"event": "edit", "path": path, "ok": false})
		return "Not applied: " + err.Error(), true
	}
	s.record(map[string]any{"event": "edit", "path": path, "ok": true})
	// The file changed: rebuild its blocks, keeping what was shown by name
	// and showing the edited region.
	if f, ok := s.files[path]; ok {
		labels := map[string]bool{}
		for _, b := range f.doc.Blocks {
			if f.kept[b.ID] {
				labels[b.Label] = true
			}
		}
		delete(s.files, path)
		if nf, err := s.load(path); err == nil {
			nf.ranked, nf.focus = f.ranked, f.focus
			if !f.ranked {
				keepAll(nf)
			}
			src, _ := os.ReadFile(filepath.Join(*work, path))
			at := strings.Index(string(src), replace)
			line := strings.Count(string(src[:max(at, 0)]), "\n") + 1
			end := line + strings.Count(replace, "\n")
			for _, b := range nf.doc.Blocks {
				if labels[b.Label] || (at >= 0 && b.Start <= end && b.End >= line) {
					nf.kept[b.ID] = true
				}
			}
		}
	}
	return "Applied.", false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func tools() []map[string]any {
	p := func(t, d string) map[string]any { return map[string]any{"type": t, "description": d} }
	read := "Read a file of the repository. Lines are numbered (\"  12| code\"); the numbers are not part of the code."
	readProps := map[string]any{"path": p("string", "path relative to the repository root")}
	if *mode == "cola" {
		read += " Long files are shown in part to save reading: each hidden run is one line \"… [eN: lines A-B hidden: names]\" naming every function in it. Use show to see hidden code you need. Reading the same file again returns the same view unless you give a new focus."
		readProps["focus"] = p("string", "optional: what you are looking for in this file; the visible part is chosen for it")
	}
	ts := []map[string]any{
		{"name": "read_file", "description": read, "inputSchema": map[string]any{"type": "object", "properties": readProps, "required": []string{"path"}}},
		{"name": "edit", "description": "Replace one exact, unique block of text in a file. Copy \"search\" verbatim from the code you read, including indentation, without the line numbers.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"path": p("string", "path relative to the repository root"), "search": p("string", "exact text to replace"), "replace": p("string", "new text")},
				"required": []string{"path", "search", "replace"}}},
	}
	if *mode == "cola" {
		ts = append(ts, map[string]any{"name": "show", "description": "Show hidden parts of a file read with read_file: the functions a hidden-run marker names, or a line range.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"path":      p("string", "path relative to the repository root"),
				"functions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "names from the hidden-run markers"},
				"lines":     p("string", "optional line range like \"120-180\"")},
				"required": []string{"path"}}})
	}
	return ts
}

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	flag.Parse()
	s := &server{env: &bench.SWEEnv{Python: *python, RepoRoot: *root}, files: map[string]*file{}}
	if *taskFile != "" {
		b, err := os.ReadFile(*taskFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s.task = string(b)
	}
	if *ledger != "" {
		f, err := os.OpenFile(*ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s.log = f
	}
	ctx := context.Background()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for in.Scan() {
		var m rpc
		if json.Unmarshal(in.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue // notifications need no answer
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			reply(m.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "colamcp", "version": "0.1"}})
		case "tools/list":
			reply(m.ID, map[string]any{"tools": tools()})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(m.Params, &p)
			var text string
			var isErr bool
			switch p.Name {
			case "read_file":
				text, isErr = s.readFile(ctx, p.Arguments)
			case "show":
				text, isErr = s.show(p.Arguments)
			case "edit":
				text, isErr = s.edit(p.Arguments)
			default:
				text, isErr = "unknown tool "+p.Name, true
			}
			s.record(map[string]any{"event": "tool", "name": p.Name, "result_tokens": cola.EstTokens(text), "error": isErr})
			reply(m.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr})
		case "ping":
			reply(m.ID, map[string]any{})
		default:
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}
