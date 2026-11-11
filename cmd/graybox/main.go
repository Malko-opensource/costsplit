// Command graybox is the CLI a coding agent calls (D-013).
//
//	graybox lock  [-dir DIR]
//	graybox plan  [-dir DIR] -task ID -model small|large -plan plan.json
//	graybox apply [-dir DIR] -task ID -model small|large -symbol SYM -body-file FILE
//	graybox check [-dir DIR]
//
// Every command prints one JSON result. "next" is a recommendation only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Malko-opensource/costsplit/internal/adapter"
	"github.com/Malko-opensource/costsplit/internal/decider"
	"github.com/Malko-opensource/costsplit/internal/extadapter"
	"github.com/Malko-opensource/costsplit/internal/goadapter"
	"github.com/Malko-opensource/costsplit/internal/manifest"
	"github.com/Malko-opensource/costsplit/internal/policy"
	"github.com/Malko-opensource/costsplit/internal/runner"
)

type Config struct {
	Lang       string         `json:"lang"`
	AdapterCmd []string       `json:"adapter_cmd,omitempty"`
	Decider    DeciderConfig  `json:"decider"`
	Limits     *policy.Limits `json:"limits"`
}

type DeciderConfig struct {
	URL            string `json:"url"`
	Model          string `json:"model"`
	APIKeyEnv      string `json:"api_key_env"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("dir", ".", "project root (holds .graybox/)")
	task := fs.String("task", "", "task id")
	model := fs.String("model", "small", "small|large: which model made this attempt")
	planFile := fs.String("plan", "", "plan JSON file")
	symbol := fs.String("symbol", "", "symbol whose body to replace")
	bodyFile := fs.String("body-file", "", "file holding the new body ('-' = stdin)")
	_ = fs.Parse(os.Args[2:])

	r, err := build(*dir)
	if err != nil {
		die(err)
	}
	ctx := context.Background()
	var res runner.Result
	switch cmd {
	case "lock":
		res, err = r.Lock()
	case "check":
		res, err = r.Check()
	case "plan":
		need(*task != "" && *planFile != "", "plan needs -task and -plan")
		var p *manifest.Plan
		if p, err = manifest.LoadPlan(*planFile); err == nil {
			res, err = r.Plan(ctx, *task, policy.Model(*model), p)
		}
	case "apply":
		need(*task != "" && *symbol != "" && *bodyFile != "", "apply needs -task, -symbol, -body-file")
		var body []byte
		if *bodyFile == "-" {
			body, err = io.ReadAll(os.Stdin)
		} else {
			body, err = os.ReadFile(*bodyFile)
		}
		if err == nil {
			res, err = r.Apply(ctx, *task, policy.Model(*model), *symbol, string(body))
		}
	default:
		usage()
	}
	if err != nil && res.Error == "" {
		res.Error = err.Error()
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if err != nil {
		os.Exit(1)
	}
}

func build(dir string) (*runner.Runner, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(abs, runner.Dot, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Limits == nil || c.Limits.SmallRetries < 1 || c.Limits.LargeRetries < 1 || c.Limits.ConfidenceThreshold <= 0 {
		return nil, fmt.Errorf("config.limits must set small_retries, large_retries and confidence_threshold (no defaults: spec Q-007, Q-012)")
	}
	a, err := makeAdapter(c, abs)
	if err != nil {
		return nil, err
	}
	d := &decider.Client{URL: c.Decider.URL, Model: c.Decider.Model, Timeout: time.Duration(c.Decider.TimeoutSeconds) * time.Second}
	if c.Decider.APIKeyEnv != "" {
		d.APIKey = os.Getenv(c.Decider.APIKeyEnv)
	}
	return &runner.Runner{Dir: abs, A: a, D: d, Limits: *c.Limits}, nil
}

func makeAdapter(c Config, dir string) (adapter.Adapter, error) {
	if c.Lang == "go" {
		return goadapter.New(dir), nil
	}
	cmd := c.AdapterCmd
	if len(cmd) == 0 {
		home := os.Getenv("GRAYBOX_HOME")
		if home == "" {
			return nil, fmt.Errorf("lang %q needs adapter_cmd in config or GRAYBOX_HOME", c.Lang)
		}
		switch c.Lang {
		case "python":
			cmd = []string{"python3", filepath.Join(home, "adapters/python/adapter.py")}
		case "ts":
			cmd = []string{"node", filepath.Join(home, "adapters/ts/adapter.mjs")}
		case "java":
			cmd = []string{filepath.Join(home, "adapters/jvm/java-adapter.sh")}
		case "kotlin":
			cmd = []string{filepath.Join(home, "adapters/jvm/kotlin-adapter.sh")}
		default:
			return nil, fmt.Errorf("unknown lang %q", c.Lang)
		}
	}
	return &extadapter.Adapter{Language: c.Lang, Cmd: cmd, Dir: dir}, nil
}

func need(ok bool, msg string) {
	if !ok {
		die(fmt.Errorf("%s", msg))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "graybox:", err)
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: graybox lock|plan|apply|check [flags]")
	os.Exit(2)
}
