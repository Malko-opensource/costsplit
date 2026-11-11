// Package adapter defines what each language must provide (D-018). The core
// never branches on language; everything language-specific lives behind this.
package adapter

type Symbol struct {
	Name      string `json:"name"`
	File      string `json:"file"`
	IsStub    bool   `json:"is_stub"`
	Signature string `json:"signature"`
}

type CheckStatus string

const (
	CheckPass    CheckStatus = "pass"
	CheckFail    CheckStatus = "fail"
	CheckSkipped CheckStatus = "skipped"
)

type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type Adapter interface {
	Lang() string
	// Resolve finds a symbol and reports whether its body is still a stub.
	Resolve(symbol string) (Symbol, error)
	// Apply replaces only the body of symbol; signatures cannot change (D-014).
	Apply(symbol, body string) error
	// Checks runs format, build/type, lint and test checks.
	Checks() []Check
	// ListTests returns test names as the manifest refers to them.
	ListTests() ([]string, error)
	// APISnapshot returns the public surface plus graybox data (state tables),
	// one normalized line per item, sorted.
	APISnapshot() ([]string, error)
	// Stubs lists symbols whose bodies are still stubs, in Resolve's format.
	Stubs() ([]string, error)
	// Preconditions are checks that must pass before a graybox can be locked,
	// e.g. strict type checking for Python and JS (D-019).
	Preconditions() []Check
}

func AllPass(cs []Check) bool {
	for _, c := range cs {
		if c.Status == CheckFail {
			return false
		}
	}
	return true
}
