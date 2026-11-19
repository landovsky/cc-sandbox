// Package settings compiles a profile into the fence's managed-settings.json and
// managed-mcp.json (U17). Managed settings are the top of Claude Code's precedence: neither the
// worktree nor the agent's home can override them. They are still a speed bump (invariant 3):
// enforcement is outside the fence. Their job is to make the fence start cleanly (no login
// prompt, no update check, no Remote Control) and to keep steering files out of reach.
package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"

	"github.com/landovsky/cc-sandbox/internal/profile"
)

// BaseDeny is the fixed deny list. A profile adds to it; nothing removes from it.
// Rules use Claude Code's permission syntax. Deny rules hold even in bypassPermissions.
var BaseDeny = []string{
	// Privilege and host reach: nothing in the fence should try them, and a try is noise.
	"Bash(sudo:*)", "Bash(su:*)", "Bash(doas:*)", "Bash(ssh:*)", "Bash(scp:*)", "Bash(docker:*)", "Bash(nsenter:*)",
	// Git: commit and push go through the git shim to the outbox, never to a remote directly.
	"Bash(git push:*)", "Bash(git remote add:*)", "Bash(git config:*)",
	// Steering files (invariant 25): an agent-written hook, MCP server or setting is untrusted,
	// so the agent can't write them in the worktree or its home.
	"Edit(**/.claude/settings.json)", "Edit(**/.claude/settings.local.json)",
	"Edit(**/.claude/hooks/**)", "Edit(**/.mcp.json)", "Edit(~/.claude/**)",
	// The outbox's host-stamped inbox is read-only to the agent.
	"Edit(/run/ccsbx/in/**)",
}

// Params are the values that vary per session but not per profile.
type Params struct {
	BaseURL  string // the forwarder's model route, e.g. http://fwd:8080
	FetchURL string // the forwarder's fetch route (HTTPS_PROXY); empty = no web fetch route
	CCFCmd   string // command of the one managed MCP server, "ccf" (default /opt/ccf/bin/ccf-mcp)
}

// Result is the compiled pair of files.
type Result struct {
	Settings []byte // managed-settings.json
	MCP      []byte // managed-mcp.json
}

// Compile turns a profile into managed settings. It refuses a malformed base URL: a typo there
// would send model traffic somewhere the forwarder doesn't control.
func Compile(p *profile.Profile, par Params) (*Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := checkURL("BaseURL", par.BaseURL, true); err != nil {
		return nil, err
	}
	if err := checkURL("FetchURL", par.FetchURL, false); err != nil {
		return nil, err
	}
	if par.CCFCmd == "" {
		par.CCFCmd = "/opt/ccf/bin/ccf-mcp"
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL":                       par.BaseURL,
		"ANTHROPIC_AUTH_TOKEN":                     "dummy", // the forwarder holds the real token; this only avoids the login prompt
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_TELEMETRY":                        "1",
		"DISABLE_AUTOUPDATER":                      "1",
	}
	if par.FetchURL != "" {
		env["HTTPS_PROXY"] = par.FetchURL
	}
	s := map[string]any{
		"permissions": map[string]any{
			"deny":        denyList(p),
			"defaultMode": p.Mode,
		},
		// Only managed hooks and managed permission rules count; the worktree's can't add or widen.
		"allowManagedHooksOnly":           true,
		"allowManagedPermissionRulesOnly": true,
		// Managed MCP is `ccf` only (see managed-mcp.json); worktree .mcp.json servers never load.
		"allowedMcpServers":                 []map[string]string{{"serverName": "ccf"}},
		"enableAllProjectMcpServers":        false,
		"enabledMcpjsonServers":             []string{},
		"disableRemoteControl":              true,
		"skipWebFetchPreflight":             true, // the fence can't reach api.anthropic.com; the door's list replaces the check
		"skipDangerousModePermissionPrompt": p.Mode == "bypassPermissions",
		"env":                               env,
	}
	mcp := map[string]any{"mcpServers": map[string]any{
		"ccf": map[string]any{"type": "stdio", "command": par.CCFCmd, "args": []string{}},
	}}
	sb, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	mb, err := json.MarshalIndent(mcp, "", "  ")
	if err != nil {
		return nil, err
	}
	return &Result{Settings: append(sb, '\n'), MCP: append(mb, '\n')}, nil
}

// denyList is base + profile, deduplicated, base entries first.
func denyList(p *profile.Profile) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range BaseDeny {
		seen[r] = true
		out = append(out, r)
	}
	extra := append([]string(nil), p.Deny...)
	sort.Strings(extra)
	for _, r := range extra {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func checkURL(what, v string, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%s is required", what)
		}
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q must be an http(s) URL with a host", what, v)
	}
	return nil
}
