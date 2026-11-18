#!/usr/bin/env python3
"""Spin-up benchmark: how long until a new Claude Code session is ready for its first
prompt, and until its first answer, in a docker sandbox vs a plain host start.

Throwaway spike for ai-tooling:#4yc5. Nothing here holds a real credential: the model
is a local stub (stub.py) that answers every request with "STUB-OK"; Claude Code gets
ANTHROPIC_AUTH_TOKEN=ccflow-placeholder. Only arm `real` uses Tomas's own login, and it
never sends a prompt (ready-for-prompt only, zero tokens).

Arms
  sbx   cold sandbox: `docker run` per start (python:3.12-slim, the host's claude binary
        bind-mounted read-only, uid 10001, read-only rootfs, cap-drop ALL,
        no-new-privileges, pids/memory limits, an --internal docker network whose only
        other member is the stub container: no internet, no DNS outside it)
  warm  warm sandbox: `docker exec` into an already-running container of the same shape
        (the "warm pool" option)
  host  same binary, same env, same stub, no container (isolates the container's cost)
  real  today's binary with the real login and network, user settings (hooks) skipped

How it times
  Every start runs in its own window of a private tmux server (-L ccsbx), like a cc-tree
  tab. t0 = just before `tmux new-window`. ready = the screen shows the empty prompt line
  and the mode line ("bypass permissions"). answer = after `say hi` + Enter, the screen
  shows "● STUB-OK" (ready + typing-to-answer; the --settle pause is excluded). Arms host and real stop
  at ready: on hp the root-owned managed settings pin ANTHROPIC_BASE_URL to ccsp, so a
  host process can't reach the stub (the placeholder gets a 401 from Anthropic).
  Polls capture-pane every 30 ms.

Recipes
  ./bench.py setup                 # network, stub container, host stub, warm container
  ./bench.py run sbx,warm,host,real -n 10 > results.jsonl   # interleaved, 10 rounds
  ./bench.py summary results.jsonl
  ./bench.py teardown              # removes every ccsbx-* container, the network, the
                                   # tmux server and the host stub; deletes .state/

Tunables (flag, else env var)
  --claude-bin / CCSBX_CLAUDE_BIN   claude binary (default: ~/.local/bin/claude resolved)
  --image      / CCSBX_IMAGE        sandbox base image (default python:3.12-slim)
  --port       / CCSBX_HOST_PORT    host stub port on 127.0.0.1 (default 18080)
  --state      / CCSBX_STATE        per-run homes and the shared /work (default spike/.state)
  -n                                rounds (default 10)
  --settle     / CCSBX_SETTLE       pause between ready and typing, excluded from answer (default 0.3)
"""
import argparse, json, os, re, shlex, shutil, signal, statistics as st, subprocess as sp, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
NET, STUB, WARM, SOCK = "ccsbx-net", "ccsbx-stub", "ccsbx-warm", "ccsbx"
ENV_SBX = {"HOME": "/home/agent", "TERM": "xterm-256color", "LANG": "C.UTF-8",
           "ANTHROPIC_BASE_URL": f"http://{STUB}:8080", "ANTHROPIC_AUTH_TOKEN": "ccflow-placeholder",
           "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1"}
FENCE = ["--user", "10001:10001", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
         "--pids-limit", "512", "--memory", "2g", "--tmpfs", "/tmp:rw,exec,size=256m", "--network", NET]


def cfg():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("cmd", choices=["setup", "run", "summary", "teardown"])
    p.add_argument("arg", nargs="?", help="run: comma-separated arms; summary: results file")
    p.add_argument("-n", type=int, default=10)
    p.add_argument("--settle", type=float, default=float(os.environ.get("CCSBX_SETTLE", 0.3)))
    p.add_argument("--claude-bin", default=os.environ.get("CCSBX_CLAUDE_BIN") or os.path.realpath(Path.home() / ".local/bin/claude"))
    p.add_argument("--image", default=os.environ.get("CCSBX_IMAGE", "python:3.12-slim"))
    p.add_argument("--port", type=int, default=int(os.environ.get("CCSBX_HOST_PORT", 18080)))
    p.add_argument("--state", type=Path, default=Path(os.environ.get("CCSBX_STATE", HERE / ".state")))
    return p.parse_args()


def sh(*a, **kw): return sp.run(a, capture_output=True, text=True, **kw)
def tm(*a): return sh("tmux", "-L", SOCK, *a).stdout


def seed_home(a, home: Path, workdir: str):
    """A fresh $HOME with onboarding, trust and the bypass prompt pre-answered, so the
    start goes straight to the prompt (the ~/.claude.json keys doc 68 §6.1 marked unknown)."""
    (home / ".claude").mkdir(parents=True, exist_ok=True)
    (home / ".claude.json").write_text(json.dumps({
        "hasCompletedOnboarding": True, "theme": "dark", "numStartups": 5, "bypassPermissionsModeAccepted": True,
        "projects": {workdir: {"hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True}}}))
    (home / ".claude/settings.json").write_text(json.dumps({"skipDangerousModePermissionPrompt": True}))
    os.chmod(home, 0o777); os.chmod(home / ".claude", 0o777)
    for f in (home / ".claude.json", home / ".claude/settings.json"): os.chmod(f, 0o666)


def docker_mounts(a, home):
    return ["-v", f"{home}:/home/agent", "-v", f"{a.state / 'work'}:/work", "-w", "/work",
            "-v", f"{a.claude_bin}:/usr/local/bin/claude:ro"]


def setup(a):
    (a.state / "work").mkdir(parents=True, exist_ok=True); os.chmod(a.state / "work", 0o777)
    if sh("docker", "network", "inspect", NET).returncode: sh("docker", "network", "create", "--internal", NET)
    if sh("docker", "inspect", STUB).returncode:
        sh("docker", "run", "-d", "--name", STUB, "--network", NET, "-v", f"{HERE / 'stub.py'}:/stub.py:ro",
           a.image, "python", "-u", "/stub.py", "0.0.0.0", "8080")
    pidf = a.state / "host-stub.pid"
    if not pidf.exists():
        log = open(a.state / "host-stub.log", "a")
        pr = sp.Popen([sys.executable, str(HERE / "stub.py"), "127.0.0.1", str(a.port)], stdout=log, stderr=log,
                      start_new_session=True)
        pidf.write_text(str(pr.pid))
    if sh("docker", "inspect", WARM).returncode:
        home = a.state / "runs/warm-home"; seed_home(a, home, "/work")
        r = sh("docker", "run", "-d", "--name", WARM, *FENCE, *docker_mounts(a, home), a.image, "sleep", "infinity")
        if r.returncode: sys.exit(r.stderr)
    if not tm("list-sessions"):
        sh("tmux", "-L", SOCK, "-f", "/dev/null", "new-session", "-d", "-s", "s", "-x", "200", "-y", "50")
    print("setup done:", NET, STUB, WARM, f"host stub 127.0.0.1:{a.port}", f"tmux -L {SOCK}")


def command(a, arm, rid):
    q = shlex.quote
    if arm == "sbx":
        home = a.state / "runs" / rid; seed_home(a, home, "/work")
        env = sum((["-e", f"{k}={v}"] for k, v in ENV_SBX.items()), [])
        return " ".join(q(x) for x in ["docker", "run", "--rm", "-it", "--name", f"ccsbx-{rid}", *FENCE,
                                       *docker_mounts(a, home), *env, a.image, "claude", "--dangerously-skip-permissions"])
    if arm == "warm":
        env = sum((["-e", f"{k}={v}"] for k, v in ENV_SBX.items()), [])
        return " ".join(q(x) for x in ["docker", "exec", "-it", "-u", "10001:10001", "-w", "/work", *env, WARM,
                                       "claude", "--dangerously-skip-permissions"])
    work = str(a.state / "work")
    if arm == "host":
        home = a.state / "runs" / rid; seed_home(a, home, work)
        env = {**ENV_SBX, "HOME": str(home), "PATH": "/usr/bin:/bin", "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{a.port}"}
        return f"cd {q(work)} && exec env -i " + " ".join(q(f"{k}={v}") for k, v in env.items()) + \
            f" {q(a.claude_bin)} --dangerously-skip-permissions"
    if arm == "real":
        skip = q(json.dumps({"skipDangerousModePermissionPrompt": True}))
        return (f"cd {q(work)} && exec env -u TMUX -u TMUX_PANE {q(a.claude_bin)} --setting-sources project,local "
                f"--settings {skip} --dangerously-skip-permissions")
    sys.exit(f"unknown arm {arm}")


READY = re.compile(r"^❯[ \u00a0]*$", re.M)  # the TUI draws ❯ + NBSP


def wait(w, pred, t0, limit):
    while time.monotonic() - t0 < limit:
        if pred(tm("capture-pane", "-p", "-t", w)): return round(time.monotonic() - t0, 3)
        time.sleep(0.03)
    return None


def one(a, arm, rid):
    cmd = command(a, arm, rid)
    t0 = time.monotonic()
    w = tm("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "s:", cmd).strip()
    ready = wait(w, lambda s: READY.search(s) and "bypass permissions" in s, t0, 60)
    ans = None
    if ready is not None and arm in ("sbx", "warm"):  # host/real: hp's managed settings pin the base URL to ccsp
        time.sleep(a.settle)
        t1 = time.monotonic()
        tm("send-keys", "-t", w, "-l", "say hi"); tm("send-keys", "-t", w, "Enter")
        x = wait(w, lambda s: "● STUB-OK" in s, t1, 30)
        ans = round(ready + x, 3) if x is not None else None
    tm("send-keys", "-t", w, "C-c"); time.sleep(0.15); tm("send-keys", "-t", w, "C-c"); time.sleep(0.6)
    tm("kill-window", "-t", w)
    return {"arm": arm, "run": rid, "ready": ready, "answer": ans}


def run(a):
    for i in range(a.n):
        for arm in a.arg.split(","):
            print(json.dumps(one(a, arm, f"{arm}{i}-{os.getpid()}")), flush=True); time.sleep(1)


def summary(a):
    rows = [json.loads(l) for l in open(a.arg) if l.strip()]
    print(f"{'arm':6} {'n':>3} {'ready med':>9} {'min':>6} {'max':>6} {'answer med':>10} {'min':>6} {'max':>6}")
    for arm in dict.fromkeys(r["arm"] for r in rows):
        rs = [r for r in rows if r["arm"] == arm]
        def f(k):
            v = sorted(r[k] for r in rs if r[k] is not None)
            return (f"{st.median(v):9.2f} {v[0]:6.2f} {v[-1]:6.2f}" if v else f"{'-':>9} {'-':>6} {'-':>6}")
        print(f"{arm:6} {len(rs):3} {f('ready')} {f('answer'):>24}")


def teardown(a):
    ids = sh("docker", "ps", "-aq", "--filter", "name=ccsbx-").stdout.split()
    if ids: sh("docker", "rm", "-f", *ids)
    sh("docker", "network", "rm", NET); sh("tmux", "-L", SOCK, "kill-server")
    pidf = a.state / "host-stub.pid"
    if pidf.exists():
        try: os.kill(int(pidf.read_text()), signal.SIGTERM)
        except ProcessLookupError: pass
    if a.state.exists():  # files uid 10001 wrote: remove them from inside a container
        sh("docker", "run", "--rm", "-v", f"{a.state}:/s", a.image, "sh", "-c", "rm -rf /s/runs /s/work")
        shutil.rmtree(a.state, ignore_errors=True)
    print("teardown done")


if __name__ == "__main__":
    a = cfg()
    {"setup": setup, "run": run, "summary": summary, "teardown": teardown}[a.cmd](a)
