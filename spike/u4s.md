# U4s spike, half 1 (no token): subscription and web search through a base-URL proxy

What this is: the findings of the first half of U4s (cc-sandbox:#3y22, build plan row U4s,
fenced-day unknown 1), plus the proxy script `spike/u4s-proxy.py` it built.
Why it exists: the model door (U4) has to carry the subscription. Before anyone builds the door,
we need to know what an OAuth Claude Code sends to a base-URL gateway, whether web search
survives the gateway, and whether an injected setup-token works and can be revoked.

**Run on hp-ubuntu, 2026-11-21, Claude Code 2.1.330.** No installs, no root, no k3s, no docker.

## Result: the measurement half is BLOCKED, the tool is ready

| Question | Answer | Evidence |
|---|---|---|
| Does an unfenced subscription Claude Code work through a base-URL proxy? | **not measured**. hp has no live subscription login | `claude auth status` (fresh process): `loggedIn: false`, `authMethod: none`, `allowedProviders: [customEndpoint]`. `claude -p 'say hi'` via the proxy: *"Failed to authenticate: this login reached its time limit and cannot be renewed"*. It fails before any network call; the proxy saw 0 requests |
| Which `anthropic-beta` value(s) does the OAuth path send? | **not measured** (same cause) | — |
| Does web search work through the proxy? | **not measured** (same cause) | — |
| Does the proxy forward, stream and keep secrets out of its log? | **yes** | self-test with random fake `Authorization`, `x-api-key` and `Cookie` values: upstream 401 passed through, a chunked request body forwarded, the request's server tool (`web_search_20250305`) and both `anthropic-beta` headers logged, **0 bytes of the fake secrets in the log** |
| Does inject mode (dummy-token client, proxy injects the bearer) work? | **the mechanics yes, auth not proven** | client sends `Bearer dummy` + `anthropic-beta: client-beta-x`; the proxy drops it and sends `Bearer <file token>` + `client-beta-x,oauth-2025-04-20`. Upstream answers *"OAuth access token is invalid."*, so an `sk-ant-oat01-` bearer reaches Anthropic's OAuth check through a gateway |

Two side findings for U4 (`ccsp-model`):

- With both headers present, upstream checks **`x-api-key` first** (*"invalid x-api-key"*).
  So the door must strip an inbound `x-api-key` before it injects a bearer. U4 already says so;
  now it's measured.
- An oat01 bearer gets the same *"OAuth access token is invalid."* with or without
  `oauth-2025-04-20`. So a fake token can't tell whether the beta is needed. Only a real
  token can.

Why the parent session still works (likely, not checked: the credentials file is off-limits to me): it was started earlier and holds a login in
memory. The on-disk login a new process reads has expired. That is also a warning: on hp, any
new unfenced session will fail the same way until someone logs in again.

## What unblocks it (either one; both are Tomas's)

1. **R5b**: a `claude setup-token`. This also covers half 1. Run the unfenced client with
   `CLAUDE_CODE_OAUTH_TOKEN` in its env (observe mode) to record the beta values the real OAuth
   path sends, then run the same token in inject mode (half 2). Do the injection on hp from a
   0600 file, never in git (❓ B in fenced-day-unknowns).
2. Or `claude auth login` on hp (browser). This unblocks half 1 only, and it also fixes the
   expired login above. Half 2 still needs R5b.

## The second half, once R5b exists

1. Observe: `U4S_LOG=… python3 -I spike/u4s-proxy.py`, then
   `CLAUDE_CODE_OAUTH_TOKEN=$(cat ~/.u4s-token) ANTHROPIC_BASE_URL=http://127.0.0.1:47199 claude -p 'say hi'`
   and one web-search prompt. Read `beta`, `cred.authorization.scheme`, `body.server_tools`,
   `resp_scan`.
2. Inject: `--inject-token-file ~/.u4s-token --add-beta <the value(s) from step 1>`. Client:
   `ANTHROPIC_AUTH_TOKEN=dummy`, no OAuth env, a scratch `CLAUDE_CONFIG_DIR` so it can't pick up
   a login. `say hi` + web search. Then once with `--add-beta ""` to see if the beta is needed.
3. Revoke: claude.ai → Settings → Claude Code → revoke that token. Loop one `say hi` (haiku,
   `max_tokens` small, via curl to the proxy) every 30 s until it returns 401, for at most 60 min.
   Record the time from the revoke click to the first 401. A **second** token is needed for the
   real door afterwards, because this one is spent.
4. Fill in this doc's table and close U4s.

**Time once the token exists:** about 1 h of agent time plus up to 1 h of wall-clock for the revoke
poll. It needs one click by Tomas (the revoke) at a time we agree on. Spend: 4 to 6 short prompts.

## Files

- `spike/u4s-proxy.py`: the proxy (`--help` has the recipes). Observe mode is the default;
  `--inject-token-file` + `--add-beta` give inject mode. Stdlib Python, run with `python3 -I`.
  Never logs a credential value. It logs the scheme word, the prefix up to the second `-` and
  the length. A guard asserts that no `sk-ant-…` token pattern is in a log line before it
  writes the line.
