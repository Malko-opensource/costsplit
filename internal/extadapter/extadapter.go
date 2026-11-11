// Package extadapter runs a language adapter as a subprocess. Protocol: the
// command is started with the operation name as its last argument, reads one
// JSON object on stdin, and writes one JSON object on stdout:
//
//	resolve       {"symbol"}          -> {"symbol":{name,file,is_stub,signature}} | {"error"}
//	apply         {"symbol","body"}   -> {} | {"error"}
//	checks        {}                  -> {"checks":[{name,status,detail}]}
//	preconditions {}                  -> {"checks":[...]}
//	list_tests    {}                  -> {"tests":[...]}
//	api_snapshot  {}                  -> {"lines":[...]}
//	stubs         {}                  -> {"stubs":[...]}
//
// The working directory is the project root.
package extadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"

	"github.com/local/graybox/internal/adapter"
)

type Adapter struct {
	Language string
	Cmd      []string
	Dir      string
}

type reply struct {
	Error  string          `json:"error,omitempty"`
	Symbol adapter.Symbol  `json:"symbol"`
	Checks []adapter.Check `json:"checks"`
	Tests  []string        `json:"tests"`
	Lines  []string        `json:"lines"`
	Stubs  []string        `json:"stubs"`
}

func (a *Adapter) call(op string, in any) (reply, error) {
	var r reply
	b, err := json.Marshal(in)
	if err != nil {
		return r, err
	}
	args := append(append([]string{}, a.Cmd[1:]...), op)
	cmd := exec.Command(a.Cmd[0], args...)
	cmd.Dir = a.Dir
	cmd.Stdin = bytes.NewReader(b)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return r, fmt.Errorf("%s adapter %s: %v: %s", a.Language, op, err, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return r, fmt.Errorf("%s adapter %s: bad output: %v: %s", a.Language, op, err, stdout.String())
	}
	if r.Error != "" {
		return r, fmt.Errorf("%s", r.Error)
	}
	return r, nil
}

func (a *Adapter) Lang() string { return a.Language }

func (a *Adapter) Resolve(symbol string) (adapter.Symbol, error) {
	r, err := a.call("resolve", map[string]string{"symbol": symbol})
	return r.Symbol, err
}

func (a *Adapter) Apply(symbol, body string) error {
	_, err := a.call("apply", map[string]string{"symbol": symbol, "body": body})
	return err
}

func (a *Adapter) Checks() []adapter.Check {
	r, err := a.call("checks", struct{}{})
	if err != nil {
		return []adapter.Check{{Name: "adapter", Status: adapter.CheckFail, Detail: err.Error()}}
	}
	return r.Checks
}

func (a *Adapter) Preconditions() []adapter.Check {
	r, err := a.call("preconditions", struct{}{})
	if err != nil {
		return []adapter.Check{{Name: "adapter", Status: adapter.CheckFail, Detail: err.Error()}}
	}
	return r.Checks
}

func (a *Adapter) ListTests() ([]string, error) {
	r, err := a.call("list_tests", struct{}{})
	sort.Strings(r.Tests)
	return r.Tests, err
}

func (a *Adapter) APISnapshot() ([]string, error) {
	r, err := a.call("api_snapshot", struct{}{})
	sort.Strings(r.Lines)
	return r.Lines, err
}

func (a *Adapter) Stubs() ([]string, error) {
	r, err := a.call("stubs", struct{}{})
	sort.Strings(r.Stubs)
	return r.Stubs, err
}
