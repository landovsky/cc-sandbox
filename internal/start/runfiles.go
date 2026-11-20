// The files a fence generation starts with, built from the session record. `ccsbx run` (direct
// mode) and `ccsbx-root run` (helper mode) must hand the fence the same inputs, so there is one
// function for them instead of two copies that could drift.
package start

import "encoding/json"

// FenceFiles are the files written under the generation directory before the container starts:
// managed/ is the compiled managed settings and MCP config, in/ is the session identity, scope
// and brief. Paths are relative to the generation directory.
func (r *Record) FenceFiles(machine string, managedSettings, managedMCP []byte) map[string][]byte {
	sess, _ := json.MarshalIndent(map[string]any{
		"id": r.Reply.ID, "sid": r.Sid, "gen": r.Gen, "label": r.Label, "machine": machine,
		"owner": r.Spec.Owner, "billing": r.Spec.Billing, "profile": r.Spec.Profile, "bus_id": "",
	}, "", "  ")
	files := map[string][]byte{
		"managed/managed-settings.json": managedSettings, "managed/managed-mcp.json": managedMCP,
		"in/session.json": sess, "in/scope.json": append(append([]byte(nil), r.Spec.MsgScope...), '\n'),
	}
	if r.Spec.Brief != nil {
		files["in/brief.md"] = []byte(r.Spec.Brief.Text + "\n")
	}
	return files
}
