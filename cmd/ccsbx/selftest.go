package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/rules"
)

func selftestCmd() *cli.Command {
	return &cli.Command{
		Name:    "selftest",
		Summary: "run the allow/block rule table (tests/rules.toml) on a backend (U19a)",
		Recipes: `  ccsbx selftest --list                        # every rule, and how many lack a twin
  ccsbx selftest --suites fence --backend docker --json
  ccsbx selftest --strict                      # pending twins count as failures
  CCSBX_ROOT=direct ccsbx selftest ...         # dev mode: results are marked evidence=false`,
		Setup: func(f *cli.Flags) {
			f.String("rules", "tests/rules.toml", "the rule table; twins run from its parent's parent (the repo root)")
			f.String("suites", "", "comma-separated suites to run (default all)")
			f.String("backend", "docker", "backend to run for: docker | k8s | unit")
			f.String("where", "", "only rules for this machine class (hp|mac|mac-kubeconfig); default all")
			f.String("root", "direct", "privilege mode of this run; anything but 'helper' is not evidence (invariant 14)")
			f.Int("timeout", 120, "seconds allowed per twin")
			f.Bool("list", false, "list rules and validate the table, run nothing")
			f.Bool("strict", false, "pending twins fail the run")
			f.Bool("json", false, "machine-readable output")
		},
		Run: runSelftest,
	}
}

type selftestReport struct {
	Evidence bool           `json:"evidence"`
	Root     string         `json:"root"`
	Backend  string         `json:"backend"`
	Rules    int            `json:"rules"`
	Missing  int            `json:"rules_missing_a_twin"`
	Problems []string       `json:"problems,omitempty"`
	Counts   map[string]int `json:"counts"`
	Results  []rules.Result `json:"results"`
}

func runSelftest(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	path := get("rules")
	all, err := rules.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx selftest:", err)
		return cli.ExitFail
	}
	problems := rules.Validate(all)
	missing := 0
	for _, p := range problems {
		if strings.Contains(p, "no allow") || strings.Contains(p, "no block") {
			missing++
		}
	}
	root := get("root")
	rep := selftestReport{
		Evidence: root == "helper", // invariant 14 is waived in direct mode
		Root:     root, Backend: get("backend"), Rules: len(all), Missing: missing,
		Problems: problems, Counts: map[string]int{}, Results: []rules.Result{},
	}
	asJSON := get("json") == "true"
	emit := func() int {
		if asJSON {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Fprintln(stdout, string(b))
		}
		return exitFor(rep, get("strict") == "true")
	}

	if len(problems) > 0 { // a broken table runs nothing: its results would mean nothing
		if !asJSON {
			fmt.Fprintf(stdout, "rules.toml invalid (%d rules missing a twin):\n", missing)
			for _, p := range problems {
				fmt.Fprintln(stdout, "  -", p)
			}
		}
		return emit()
	}
	var suites []string
	if s := get("suites"); s != "" {
		suites = strings.Split(s, ",")
	}
	sel := rules.Select(all, suites, get("backend"), get("where"))
	if get("list") == "true" {
		if !asJSON {
			fmt.Fprintf(stdout, "%d rules, %d missing a twin\n", len(all), missing)
			for _, r := range sel {
				fmt.Fprintf(stdout, "  T:%-28s %-9s %-14s allow=%s block=%s\n", r.ID, r.Suite, strings.Join(r.Backends, ","), short(r.Allow), short(r.Block))
			}
		}
		return emit()
	}
	secs := f.Lookup("timeout").Value.String()
	var n int
	fmt.Sscanf(secs, "%d", &n)
	rep.Results = rules.Run(sel, get("backend"), filepath.Dir(filepath.Dir(path)), time.Duration(n)*time.Second)
	rep.Counts = rules.Counts(rep.Results)
	if !asJSON {
		for _, r := range rep.Results {
			fmt.Fprintf(stdout, "%-8s T:%s (%s) [%s]\n", r.Status, r.ID, r.Twin, r.Suite)
			if r.Output != "" {
				fmt.Fprintln(stdout, "         "+strings.ReplaceAll(r.Output, "\n", "\n         "))
			}
		}
		fmt.Fprintf(stdout, "pass=%d fail=%d pending=%d  evidence=%v (root=%s)\n",
			rep.Counts[rules.Pass], rep.Counts[rules.Fail], rep.Counts[rules.Pend], rep.Evidence, rep.Root)
		if !rep.Evidence {
			fmt.Fprintln(stdout, "NOT EVIDENCE: direct mode, invariant 14 waived")
		}
	}
	return emit()
}

func exitFor(r selftestReport, strict bool) int {
	if len(r.Problems) > 0 || r.Counts[rules.Fail] > 0 || (strict && r.Counts[rules.Pend] > 0) {
		return cli.ExitFail
	}
	return cli.ExitOK
}

func short(s string) string {
	if len(s) > 24 {
		return s[:23] + "…"
	}
	return s
}
