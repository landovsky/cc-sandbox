// Tests for the part of ccsbx-root that is not policy: which invocations need no config file. The
// rule is that only calls that run nothing (help, version, usage) may work without the root-owned
// config; any verb, even with a typo, must refuse when the config cannot be believed.
package main

import "testing"

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
