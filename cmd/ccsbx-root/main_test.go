// Tests for the part of ccsbx-root that is not policy: which invocations need no config file. The
// rule is that only calls that run nothing (help, version, usage) may work without the root-owned
// config; any verb, even with a typo, must refuse when the config cannot be believed.
package main

import (
	"path/filepath"
	"testing"
)

func TestRunsNothing(t *testing.T) {
	yes := [][]string{nil, {"--help"}, {"-h"}, {"help"}, {"version"}, {"--version"}, {"run", "--help"}, {"attach", "-h"}, {"gc", "--help"}}
	no := [][]string{{"run", "s0abcdefgh"}, {"attach", "s0abcdefgh"}, {"gc"}, {"run", "--", "--help"}, {"run", "s0abcdefgh", "--tty"}, {"bash"}, {"gc", "--", "-h"}}
	for _, a := range yes {
		if !runsNothing(a) {
			t.Errorf("%q should not need the config", a)
		}
	}
	for _, a := range no {
		if runsNothing(a) {
			t.Errorf("%q must not run without the config", a)
		}
	}
}

// hostd treats anything but exit 0 as a refusal, and a config that cannot be believed is "could not tell" (3), with a
// fixed line, not the generic refusal (2) the other verbs give
func TestWorktreeCheckWithoutABelievableConfigIsCouldNotTell(t *testing.T) {
	old := configPath
	configPath = filepath.Join(t.TempDir(), "no-such-config.toml")
	defer func() { configPath = old }()
	if c := realMain([]string{"worktree-check", "--", "/home/x/git/p"}); c != 3 {
		t.Errorf("exit %d, want 3", c)
	}
	// N3: --help runs nothing, so it works without a config for this verb as for every other
	if c := realMain([]string{"worktree-check", "--help"}); c != 0 {
		t.Errorf("worktree-check --help without a config: exit %d, want 0", c)
	}
	if c := realMain([]string{"run", "s0abcdefgh"}); c != 2 {
		t.Errorf("another verb: exit %d, want 2", c)
	}
}
