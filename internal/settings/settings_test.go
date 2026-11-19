package settings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/profile"
)

func prof(mode string, deny ...string) *profile.Profile {
	return &profile.Profile{Name: "dev-default", Machines: []string{"hp-ubuntu"}, Billing: []string{"api"},
		MemoryMB: 4096, Pids: 512, Mode: mode, Deny: deny}
}

var par = Params{BaseURL: "http://fwd:8080", FetchURL: "http://fwd:8081"}

func compiled(t *testing.T, p *profile.Profile) (settings, mcp map[string]any) {
	t.Helper()
	r, err := Compile(p, par)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.MCP, &mcp); err != nil {
		t.Fatal(err)
	}
	return
}

func denies(s map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, x := range s["permissions"].(map[string]any)["deny"].([]any) {
		out[x.(string)] = true
	}
	return out
}

// T:deny-under-bypass — allow twin. The fence runs agents in bypassPermissions (the fence, not
// prompts, is the control), so bypass must really be on, with no first-run confirmation that
// would stall a fresh session; and ordinary work isn't denied.
func TestDenyUnderBypassAllow(t *testing.T) {
	s, _ := compiled(t, prof("bypassPermissions"))
	if s["permissions"].(map[string]any)["defaultMode"] != "bypassPermissions" {
		t.Fatal("mode not bypassPermissions")
	}
	if s["skipDangerousModePermissionPrompt"] != true {
		t.Fatal("a fresh fence would stop at the bypass confirmation")
	}
	d := denies(s)
	for _, ok := range []string{"Bash(go test:*)", "Bash(git commit:*)", "Edit(src/main.go)"} {
		if d[ok] {
			t.Errorf("ordinary work %s is denied", ok)
		}
	}
}

// T:deny-under-bypass — block twin. Deny rules are what still stops `sudo` and steering-file
// edits when permission prompts are off, and only managed rules may count, so a worktree
// settings file can't widen them. A profile may add denials but never remove a base one.
func TestDenyUnderBypassBlock(t *testing.T) {
	s, _ := compiled(t, prof("bypassPermissions", "Bash(curl:*)"))
	d := denies(s)
	for _, rule := range BaseDeny {
		if !d[rule] {
			t.Errorf("base deny %q missing from the compiled settings", rule)
		}
	}
	if !d["Bash(curl:*)"] {
		t.Error("a profile's added denial was dropped")
	}
	if s["allowManagedPermissionRulesOnly"] != true || s["allowManagedHooksOnly"] != true {
		t.Error("worktree or home settings could still add hooks or widen permissions")
	}
	for _, must := range []string{"Bash(sudo:*)", "Edit(**/.mcp.json)", "Edit(**/.claude/hooks/**)", "Bash(git push:*)"} {
		if !d[must] {
			t.Errorf("%s not denied", must)
		}
	}
}

// T:worktree-mcp-ignored — allow twin. The one managed server, ccf, is the agent's door to
// CC Flow verbs, so it must be declared and allowed.
func TestWorktreeMCPIgnoredAllow(t *testing.T) {
	s, m := compiled(t, prof("bypassPermissions"))
	servers := m["mcpServers"].(map[string]any)
	if _, ok := servers["ccf"]; !ok {
		t.Fatal("managed-mcp.json lacks ccf")
	}
	if a := s["allowedMcpServers"].([]any); len(a) != 1 || a[0].(map[string]any)["serverName"] != "ccf" {
		t.Fatalf("allowedMcpServers = %v", a)
	}
}

// T:worktree-mcp-ignored — block twin. A prompt-injected repo can ship a .mcp.json that adds a
// server running arbitrary commands; only `ccf` may ever load.
func TestWorktreeMCPIgnoredBlock(t *testing.T) {
	s, m := compiled(t, prof("bypassPermissions"))
	if s["enableAllProjectMcpServers"] != false {
		t.Error("project MCP servers are auto-enabled")
	}
	if e := s["enabledMcpjsonServers"].([]any); len(e) != 0 {
		t.Errorf("enabledMcpjsonServers = %v", e)
	}
	if len(m["mcpServers"].(map[string]any)) != 1 {
		t.Error("managed MCP is not ccf only")
	}
	if !denies(s)["Edit(**/.mcp.json)"] {
		t.Error("the agent may write .mcp.json")
	}
}

// A fresh fence has to start to the prompt with no login, no update check, no Remote Control and
// no preflight call to a host the fence can't reach.
func TestFreshFenceStartsWithoutPrompts(t *testing.T) {
	s, _ := compiled(t, prof("bypassPermissions"))
	env := s["env"].(map[string]any)
	want := map[string]string{
		"ANTHROPIC_BASE_URL": "http://fwd:8080", "ANTHROPIC_AUTH_TOKEN": "dummy",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_TELEMETRY": "1",
		"DISABLE_AUTOUPDATER": "1", "HTTPS_PROXY": "http://fwd:8081",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s = %v, want %s", k, env[k], v)
		}
	}
	if s["disableRemoteControl"] != true || s["skipWebFetchPreflight"] != true {
		t.Error("remote control / preflight flags not set")
	}
}

// The real token never goes into settings: the forwarder holds it (invariant 2).
func TestNoRealTokenInSettings(t *testing.T) {
	r, _ := Compile(prof("bypassPermissions"), par)
	if strings.Contains(string(r.Settings), "sk-") || strings.Contains(string(r.Settings), "eyJ") {
		t.Fatal("settings contain something token-shaped")
	}
}

func TestCompileRefusals(t *testing.T) {
	t.Run("a base URL typo would send model traffic past the forwarder", func(t *testing.T) {
		for _, u := range []string{"", "fwd:8080", "ftp://x", "http://"} {
			if _, err := Compile(prof("bypassPermissions"), Params{BaseURL: u}); err == nil {
				t.Errorf("BaseURL %q accepted", u)
			}
		}
	})
	t.Run("no fetch route means no HTTPS_PROXY rather than an empty one", func(t *testing.T) {
		r, err := Compile(prof("bypassPermissions"), Params{BaseURL: "http://fwd:8080"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(r.Settings), "HTTPS_PROXY") {
			t.Fatal("HTTPS_PROXY present")
		}
	})
	t.Run("output is byte-stable so a diff of two compiles shows real changes only", func(t *testing.T) {
		a, _ := Compile(prof("bypassPermissions", "Bash(b:*)", "Bash(a:*)"), par)
		b, _ := Compile(prof("bypassPermissions", "Bash(a:*)", "Bash(b:*)"), par)
		if string(a.Settings) != string(b.Settings) {
			t.Fatal("compile order depends on profile order")
		}
	})
}
