// Package rules loads tests/rules.toml, the allow/block catalogue (invariant 23), checks that
// every rule has both twins, and runs the twins for `ccsbx selftest`.
//
// A rule is a pair: the *allow* twin proves the legitimate path still works, the *block* twin
// proves the forbidden path is refused. A rule with only a block can be "satisfied" by a fence
// that blocks everything, so a row missing either twin fails the meta-test (and `make test`).
//
// A twin is a shell command run from the repo root; exit 0 = the twin holds. The literal value
// "pending" says the test is not built yet: it is reported as pending, never as a pass, and fails
// under `selftest --strict`.
package rules

import (
	"fmt"

	"github.com/landovsky/cc-sandbox/internal/tomlmini"

	"os"
	"sort"
	"strings"
)

// Pending marks a twin that is declared but not built yet.
const Pending = "pending"

// Known values, so a typo in a row is a meta-test failure rather than a rule that never runs.
var (
	Backends = []string{"docker", "k8s", "unit"}
	Wheres   = []string{"hp", "mac", "mac-kubeconfig", "any"}
)

// Rule is one row of tests/rules.toml.
type Rule struct {
	ID         string // without the "T:" prefix
	Title      string
	Suite      string
	Allow      string
	Block      string
	Invariants []int
	Backends   []string
	Where      string
}

// Load reads and parses a rules file. It does not validate; call Validate.
func Load(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

// Parse turns rules.toml text into rules.
func Parse(src string) ([]Rule, error) {
	doc, err := tomlmini.Parse(src)
	if err != nil {
		return nil, err
	}
	for k := range doc {
		if k != "rule" {
			return nil, fmt.Errorf("unknown table [[%s]]", k)
		}
	}
	var out []Rule
	for i, row := range doc["rule"] {
		r := Rule{}
		for k, v := range row {
			switch k {
			case "id":
				r.ID, _ = v.(string)
			case "title":
				r.Title, _ = v.(string)
			case "suite":
				r.Suite, _ = v.(string)
			case "allow":
				r.Allow, _ = v.(string)
			case "block":
				r.Block, _ = v.(string)
			case "where":
				r.Where, _ = v.(string)
			case "backends":
				r.Backends = strs(v)
			case "invariants":
				arr, ok := v.([]any)
				if !ok {
					return nil, fmt.Errorf("rule #%d: invariants must be an array of integers", i+1)
				}
				for _, x := range arr {
					n, ok := x.(int)
					if !ok {
						return nil, fmt.Errorf("rule #%d: invariants must be integers", i+1)
					}
					r.Invariants = append(r.Invariants, n)
				}
			default:
				return nil, fmt.Errorf("rule #%d (%s): unknown key %q", i+1, r.ID, k)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func strs(v any) []string {
	var out []string
	a, _ := v.([]any)
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Validate is the twin meta-test: it returns one problem per defect, empty when the catalogue
// is sound. A rule without a block (or allow) twin is the case the plan names explicitly.
func Validate(rs []Rule) []string {
	var bad []string
	seen := map[string]bool{}
	for i, r := range rs {
		who := fmt.Sprintf("rule #%d", i+1)
		if r.ID != "" {
			who = "T:" + r.ID
		}
		if r.ID == "" {
			bad = append(bad, who+": no id")
		} else if seen[r.ID] {
			bad = append(bad, who+": duplicate id")
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Allow) == "" {
			bad = append(bad, who+": no allow = twin (a rule that only blocks passes on a fence that blocks everything)")
		}
		if strings.TrimSpace(r.Block) == "" {
			bad = append(bad, who+": no block = twin (a rule that only allows proves nothing is refused)")
		}
		if r.Suite == "" {
			bad = append(bad, who+": no suite")
		}
		if len(r.Backends) == 0 {
			bad = append(bad, who+": no backends")
		}
		for _, b := range r.Backends {
			if !contains(Backends, b) {
				bad = append(bad, fmt.Sprintf("%s: unknown backend %q (known: %s)", who, b, strings.Join(Backends, ",")))
			}
		}
		if !contains(Wheres, r.Where) {
			bad = append(bad, fmt.Sprintf("%s: where = %q (known: %s)", who, r.Where, strings.Join(Wheres, ",")))
		}
		if len(r.Invariants) == 0 {
			bad = append(bad, who+": no invariants (name the invariant the rule enforces)")
		}
	}
	return bad
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Suites lists the distinct suite names, sorted.
func Suites(rs []Rule) []string {
	set := map[string]bool{}
	for _, r := range rs {
		set[r.Suite] = true
	}
	var out []string
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Select filters rules by suite list (empty = all), backend (empty = any) and where (empty = any).
func Select(rs []Rule, suites []string, backend, where string) []Rule {
	var out []Rule
	for _, r := range rs {
		if len(suites) > 0 && !contains(suites, r.Suite) {
			continue
		}
		if backend != "" && !contains(r.Backends, backend) {
			continue
		}
		if where != "" && r.Where != "any" && r.Where != where {
			continue
		}
		out = append(out, r)
	}
	return out
}
