package gitname

import (
	"strings"
	"testing"
)

func TestBranchIsRequiredPlainAndNotProtected(t *testing.T) {
	for _, ok := range []string{"ccsbx/s0abcdefgh", "work-1", "a.b_c/d-e", "x", strings.Repeat("a", 100), "Feature/X"} {
		if err := Branch(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for why, bad := range map[string]string{
		"empty": "", "main": "main", "MAIN in any case": "Main", "master": "master", "HEAD": "HEAD", "head": "head", "trunk": "trunk",
		"under refs/": "refs/heads/x", "101 characters": strings.Repeat("a", 101), "an option": "--upload-pack=x", "a leading dash": "-x",
		"a leading slash": "/x", "a dot dot": "a..b", "a trailing slash": "a/", "a .lock": "a.lock", "a double slash": "a//b",
		"a hidden component": "a/.b", "a space": "a b", "a colon": "a:b", "a tilde": "a~1", "a caret": "a^", "a newline": "a\nb", "a backslash": `a\b`,
	} {
		if Branch(bad) == nil {
			t.Errorf("%s (%q) was accepted", why, bad)
		}
	}
}

func TestRepoAndBase(t *testing.T) {
	for _, ok := range []string{"landovsky/cc-sandbox", "a/b", "A.b/c_d-e", strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100)} {
		if Repo(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "x", "a/b/c", "https://x/y", "../x", "a/../b", "-a/b", "a/-b", "a/b.git", "a b/c", strings.Repeat("a", 101) + "/b"} {
		if Repo(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"", "main", "release/1.2", strings.Repeat("a", 40), strings.Repeat("0", 64)} {
		if Base(ok) != nil {
			t.Errorf("base %q refused", ok)
		}
	}
	for _, bad := range []string{"--x", "a..b", "x.lock", "a b"} {
		if Base(bad) == nil {
			t.Errorf("base %q accepted", bad)
		}
	}
	if Pair("a/b", "feature", "feature") == nil || Pair("a/b", "main", "work/x") != nil || Pair("a/b", "main", "") == nil {
		t.Error("Pair")
	}
}
