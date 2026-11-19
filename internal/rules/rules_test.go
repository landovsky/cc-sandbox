package rules

import (
	"strings"
	"testing"
	"time"
)

const goodRow = `
[[rule]]
id = "demo"
title = "t"
suite = "fence"
allow = "true"
block = "true"
invariants = [3]
backends = ["docker"]
where = "hp"
`

func mustParse(t *testing.T, src string) []Rule {
	t.Helper()
	rs, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Invariant 23: this is the twin meta-test over the real catalogue. If someone adds a rule with
// only a block (or only an allow), `make test` goes red here.
func TestRealCatalogueHasBothTwinsOnEveryRule(t *testing.T) {
	rs, err := Load("../../tests/rules.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 {
		t.Fatal("tests/rules.toml has no rules")
	}
	for _, p := range Validate(rs) {
		t.Error(p)
	}
}

func TestMetaTest(t *testing.T) {
	t.Run("a sound row passes", func(t *testing.T) {
		if p := Validate(mustParse(t, goodRow)); len(p) != 0 {
			t.Fatal(p)
		}
	})

	t.Run("a rule row without block = fails: a fence that allows everything would pass it", func(t *testing.T) {
		src := strings.Replace(goodRow, `block = "true"`+"\n", "", 1)
		p := Validate(mustParse(t, src))
		if len(p) != 1 || !strings.Contains(p[0], "no block") {
			t.Fatalf("got %v", p)
		}
	})

	t.Run("a rule row without allow = fails: a fence that blocks everything would pass it", func(t *testing.T) {
		src := strings.Replace(goodRow, `allow = "true"`+"\n", "", 1)
		p := Validate(mustParse(t, src))
		if len(p) != 1 || !strings.Contains(p[0], "no allow") {
			t.Fatalf("got %v", p)
		}
	})

	t.Run("an empty block string is as bad as a missing one", func(t *testing.T) {
		src := strings.Replace(goodRow, `block = "true"`, `block = "  "`, 1)
		if p := Validate(mustParse(t, src)); len(p) != 1 {
			t.Fatalf("got %v", p)
		}
	})

	t.Run("a typo'd backend can't make a rule silently never run", func(t *testing.T) {
		src := strings.Replace(goodRow, `"docker"`, `"dokcer"`, 1)
		if p := Validate(mustParse(t, src)); len(p) != 1 || !strings.Contains(p[0], "unknown backend") {
			t.Fatalf("got %v", p)
		}
	})

	t.Run("duplicate ids are refused so T:<id> names one rule", func(t *testing.T) {
		if p := Validate(mustParse(t, goodRow+goodRow)); len(p) != 1 || !strings.Contains(p[0], "duplicate") {
			t.Fatalf("got %v", p)
		}
	})

	t.Run("an unknown key is a parse error, not a dropped field", func(t *testing.T) {
		if _, err := Parse(strings.Replace(goodRow, `title = "t"`, `blok = "x"`, 1)); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRun(t *testing.T) {
	mk := func(allow, block string) []Rule {
		return []Rule{{ID: "x", Suite: "fence", Allow: allow, Block: block, Backends: []string{"docker"}}}
	}
	t.Run("a twin's exit code decides pass or fail", func(t *testing.T) {
		res := Run(mk("true", "exit 3"), "docker", ".", 5*time.Second)
		if res[0].Status != Pass || res[1].Status != Fail {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("pending is reported as pending, never as a pass", func(t *testing.T) {
		res := Run(mk("pending", "pending"), "docker", ".", time.Second)
		if c := Counts(res); c[Pend] != 2 || c[Pass] != 0 {
			t.Fatalf("%v", c)
		}
	})
	t.Run("twins learn which backend and rule they run for", func(t *testing.T) {
		res := Run(mk(`test "$CCSBX_BACKEND/$CCSBX_RULE/$CCSBX_TWIN" = docker/x/allow`, "true"), "docker", ".", time.Second)
		if res[0].Status != Pass {
			t.Fatalf("%+v", res[0])
		}
	})
	t.Run("a hung twin is killed, not waited on", func(t *testing.T) {
		res := Run(mk("sleep 5", "true"), "docker", ".", 200*time.Millisecond)
		if res[0].Status != Fail || !strings.Contains(res[0].Output, "timeout") {
			t.Fatalf("%+v", res[0])
		}
	})
}

func TestSelect(t *testing.T) {
	rs := mustParse(t, goodRow)
	if len(Select(rs, []string{"fence"}, "docker", "hp")) != 1 {
		t.Fatal("should match")
	}
	if len(Select(rs, []string{"doors"}, "", "")) != 0 {
		t.Fatal("suite filter")
	}
	if len(Select(rs, nil, "k8s", "")) != 0 {
		t.Fatal("backend filter")
	}
}
