# C0c — hook parity spike (2026-11-20, hp-ubuntu, direct mode: not evidence)

Question (plan §3 C0c): do the host and the fence send Claude Code's hooks and statusLine the same
payload fields? C3's client layer (`ccf-hook`, the statusLine writer) is written against them.
Acceptance: `diff fields-host.txt fields-fence.txt` is empty.

**Status: the fence arm is done, the host arm is blocked on hp. The acceptance is therefore NOT met.**

## Fence arm (done): `spike/fields-fence.txt`, 63 field rows

`spike/c0c.sh fence`: the host's own `claude` 2.1.329 (mounted read-only) in a container with the
fence flags (uid 10001, `--read-only`, `--cap-drop ALL`, `no-new-privileges`, `--internal` network, tmpfs
HOME), against a local fake Anthropic API (`spike/c0c-stub.py`: one Bash tool call, then "STUB-OK"), with a
hook for every event whose command records only field names and JSON types (`spike/c0c-dump.py`).
Result: Claude Code runs fine in the fence flags and fires:

| Event | Fields seen (top level) |
|---|---|
| SessionStart | session_id, transcript_path, cwd, hook_event_name, source |
| UserPromptSubmit | session_id, transcript_path, cwd, permission_mode, hook_event_name, prompt, prompt_id |
| PreToolUse | + tool_name, tool_input{command,description}, tool_use_id, effort{level} |
| PostToolUse | + tool_response{stdout,stderr,interrupted,isImage,noOutputExpected}, duration_ms |
| Stop | + stop_hook_active, last_assistant_message, **background_tasks**, **session_crons**, effort |
| SessionEnd | + reason |

(see the file for every row). For C3: `Stop` carries **no usage and no last_exchange_ts**: `ccf-hook`
must read them from the transcript at `transcript_path`, as the host hook does.

Not triggered by the fake model, so **unmeasured in either arm**: `Notification` (the `notification_type`
that U3b's idle rule reads), `SubagentStop`/`SubagentStart` (the fork), `PreCompact`, and **statusLine**
(Claude Code runs it only in the interactive TUI, not in `-p`). They need a scripted TUI session.

## Host arm (blocked): why, and how to run it

`spike/c0c.sh host` refuses on hp: the root-owned managed settings pin the model base URL to the live
proxy (the same finding as `spike/bench.py`), so a host process cannot reach the fake API, and one that
did would use Tomas's real login and be billed. I did not read that file or run a real turn.

To finish, either of:
1. on a machine without managed settings (the Mac): `spike/c0c.sh host` then `spike/c0c.sh diff`; or
2. on hp, deliberately, Tomas's call (one real turn is billed): `CCSBX_C0C_ALLOW_MANAGED=1 spike/c0c.sh host`.
   Remove `hooks`/`statusLine` fields of your own from the user settings first: the script writes its own
   `HOME`, so they do not leak in.

Then `spike/c0c.sh diff` prints the differences (or "parity"). Differences that are expected and fine:
`transcript_path` values (not recorded), `prompt` text; anything else is a finding for C3.
