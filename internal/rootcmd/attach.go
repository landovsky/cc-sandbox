// `ccsbx-root attach <sid>`: the owner's way into a fenced pane. The panes live in a tmux server
// that belongs to the service uid (socket 0600 in a 0700 directory), so no other process of the
// owner's uid can type into them; this verb checks the sid against the daemon's record and the
// caller, becomes the service uid, and execs tmux attach for that one session. It takes only a sid:
// no --read-only (the caller's `ccsbx attach` refuses it first, exit 64), no target, no command.
package rootcmd

import (
	"fmt"
	"io"
	"regexp"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

var termRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,40}$`)

func (e *env) attachCmd() *cli.Command {
	return &cli.Command{
		Name:    "attach",
		Summary: "attach the caller's terminal to one session's pane (as the service uid)",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root attach s0k1dry66qg     # 'ccsbx attach <sid>' runs this for you
  Takes only the sid; --read-only and every other flag are refused (exit 64). The caller must own
  the session (or be the service user). Detach with the outer tmux's own controls.`,
		Run: func(_ *cli.Flags, args []string, _, stderr io.Writer) int {
			sid, ok := oneSid("attach", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			if err := e.attach(sid); err != nil {
				return fail(stderr, "attach", err)
			}
			return cli.ExitOK // not reached when Exec succeeds
		},
	}
}

func (e *env) attach(sid string) error {
	w, err := e.prelude("attach")
	if err != nil {
		return err
	}
	if _, err := e.session(w, sid); err != nil {
		return err
	}
	tmux, err := e.cfg.binary(e.cfg.Tmux)
	if err != nil {
		return err
	}
	uid, gid, err := e.d.LookupUser(e.cfg.ServiceUser)
	if err != nil {
		return refuse("service user %s: %v", e.cfg.ServiceUser, err)
	}
	// the environment is rebuilt: only TERM comes from the caller, and only if it looks like one
	term := e.d.Getenv("TERM")
	if !termRe.MatchString(term) {
		term = "xterm-256color"
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "TERM=" + term}
	// The server's own settings are not trusted: with a prefix key or a mouse menu the caller could
	// run tmux commands as the service uid. Every attach sets the keys off on the running server
	// first, in the same invocation, and -f /dev/null keeps a server started here from reading
	// any config.
	argv := []string{"tmux", "-S", e.cfg.TmuxSocket, "-f", "/dev/null",
		"set-option", "-g", "prefix", "None", ";",
		"set-option", "-g", "prefix2", "None", ";",
		"set-option", "-g", "mouse", "off", ";",
		"unbind-key", "-a", ";",
		"unbind-key", "-a", "-T", "root", ";",
		"attach", "-t", "=" + sid + ":"}
	if err := e.d.DropPrivs(uid, gid); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.ServiceUser, err)
	}
	return e.d.Exec(tmux, argv, env)
}

// dropPrivs becomes uid:gid with no supplementary groups, then proves root cannot be regained.
func dropPrivs(uid, gid int) error {
	if uid == 0 {
		return fmt.Errorf("refusing to attach as uid 0")
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	if err := syscall.Setuid(uid); err != nil {
		return err
	}
	if syscall.Getuid() != uid || syscall.Geteuid() != uid || syscall.Getgid() != gid || syscall.Getegid() != gid {
		return fmt.Errorf("ids did not change")
	}
	if syscall.Setuid(0) == nil {
		return fmt.Errorf("root can be regained after the drop")
	}
	return nil
}
