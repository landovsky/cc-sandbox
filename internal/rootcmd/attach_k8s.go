// `ccsbx-root attach <sid>` for a session that runs as a pod (F5): the owner's terminal is joined to
// the fence's tmux with kubectl exec. Three users are involved and none can do another's job:
//   - root checks the record, the owner and every file kubectl will read, then starts kubectl as the
//     launch user (ccsbx-launch, who alone holds the launcher kubeconfig and token) on a pty;
//   - the launch user checks the pod's live UID against the recorded one and execs kubectl;
//   - the terminal side (this process, after dropping to ccsbx-term, which has no credentials) reads the
//     pty, passes the bytes through internal/termfilter and writes the owner's terminal. The bytes are the
//     AGENT's (the pod's tmux runs as its uid), so only a process with nothing to steal parses them.
//
// The signer's own user (ccsbx-keys, which holds the machine key) takes no part. The owner's record,
// files and environment are hostile input throughout: the server and CA come from a root-owned file and
// go on kubectl's command line, and the pod name is derived from sid and generation.
package rootcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// k8sCtx is everything a pod verb needs once the record and the files have been checked: where
// kubectl is, who to become, and the cluster connection (server and CA from the root-owned file).
type k8sCtx struct {
	kubectl              string
	launchUID, launchGID int
	conn                 *k8s.ConnConfig
}

// users looks up the launch user and checks it can hold the launcher credential and nothing else: it is
// not root, not the signer's own user (the machine key is theirs), not the service user.
func (e *env) users() (launchUID, launchGID int, err error) {
	launchUID, launchGID, err = e.d.LookupUser(e.cfg.LaunchUser)
	if err != nil {
		return 0, 0, refuse("launch user %s: %v", e.cfg.LaunchUser, err)
	}
	if signerUID, _, err := e.d.LookupUser(e.cfg.SignerUser); err != nil {
		return 0, 0, refuse("signer user %s: %v", e.cfg.SignerUser, err)
	} else if signerUID == launchUID {
		return 0, 0, refuse("launch user %s is the signer's own user %s: the launcher must not hold the machine key", e.cfg.LaunchUser, e.cfg.SignerUser)
	}
	return launchUID, launchGID, nil
}

// k8sFiles checks everything kubectl will read as the launch user. It is run by root before the start and
// again by the launch user's own process before it execs kubectl, so neither trusts the other.
func (e *env) k8sFiles(launchUID int) (kubectl string, conn *k8s.ConnConfig, err error) {
	kubectl, err = e.cfg.binary(e.cfg.Kubectl)
	if err != nil {
		return "", nil, err
	}
	if err := trustedFile(e.cfg.K8sConn, e.cfg.TrustedUID, true); err != nil {
		return "", nil, refuse("k8s connection file: %v", err)
	}
	conn, err = k8s.LoadConnConfig(e.cfg.K8sConn)
	if err != nil {
		return "", nil, refuse("k8s connection file: %v", err)
	}
	if err := trustedFile(conn.CAFile, e.cfg.TrustedUID, true); err != nil {
		return "", nil, refuse("cluster CA: %v", err)
	}
	for name, p := range map[string]string{"kubeconfig": e.cfg.Kubeconfig, "launcher token": conn.TokenFile} {
		if err := launchFile(p, launchUID, e.cfg.TrustedUID); err != nil {
			return "", nil, refuse("%s %s: %v", name, p, err)
		}
	}
	return kubectl, conn, nil
}

// k8sSetup is the part every pod verb shares. Nothing here runs kubectl or reads the token: it
// checks, as root, that everything the launch user is about to read is something the owner cannot
// change, and that the launch user is not the caller, the record's owner or the service user.
func (e *env) k8sSetup(w who, rec *start.Record) (*k8sCtx, error) {
	launchUID, launchGID, err := e.users()
	if err != nil {
		return nil, err
	}
	switch {
	case launchUID == w.uid:
		return nil, refuse("launch user %s is the caller: the caller could rewrite what kubectl reads", e.cfg.LaunchUser)
	case launchUID == rec.Uid:
		return nil, refuse("launch user %s is the session's owner: the owner could rewrite what kubectl reads", e.cfg.LaunchUser)
	case launchUID == w.service:
		return nil, refuse("launch user %s is the service user: hostd would hold the launcher credential", e.cfg.LaunchUser)
	case launchGID == 0 || launchGID == w.serviceGid:
		return nil, refuse("launch user %s has primary group %d, which is root's or the service group: it would keep that group after the drop", e.cfg.LaunchUser, launchGID)
	}
	kubectl, conn, err := e.k8sFiles(launchUID)
	if err != nil {
		return nil, err
	}
	return &k8sCtx{kubectl: kubectl, launchUID: launchUID, launchGID: launchGID, conn: conn}, nil
}

// AttachSpec is what the real session runner needs: the child to start as the launch user on a pty, and
// who the terminal side becomes.
type AttachSpec struct {
	Self                 string   // the ccsbx-root binary, re-executed as `attach-exec` by the launch user
	Args                 []string // its arguments
	Env                  []string
	LaunchUID, LaunchGID int
	TermUID, TermGID     int
}

func (e *env) attachK8s(w who, rec *start.Record) (int, error) {
	// Only the owner (or root by login) gets a terminal in a pod. The service user (hostd) may start and
	// stop pod sessions but must not hold kubectl exec into every fence with the launcher credential.
	if w.uid != rec.Uid && w.uid != 0 {
		return 0, refuse("no such session %q (or it is not yours)", rec.Sid)
	}
	if rec.PodUID == "" {
		return 0, refuse("session %s has no pod UID recorded, so its pod cannot be told from another under the same name", rec.Sid)
	}
	kc, err := e.k8sSetup(w, rec)
	if err != nil {
		return 0, err
	}
	termUID, termGID, err := e.d.LookupUser(e.cfg.TermUser)
	if err != nil {
		return 0, refuse("terminal user %s: %v", e.cfg.TermUser, err)
	}
	signerUID, _, _ := e.d.LookupUser(e.cfg.SignerUser)
	switch {
	case termUID == kc.launchUID || termUID == signerUID || termUID == w.service || termUID == rec.Uid || termUID == w.uid:
		return 0, refuse("terminal user %s must be nobody else (not the launch, signer or service user, the caller or the owner): it parses the agent's bytes", e.cfg.TermUser)
	case termGID == 0 || termGID == w.serviceGid:
		return 0, refuse("terminal user %s has primary group %d, which is root's or the service group", e.cfg.TermUser, termGID)
	}
	self, err := e.d.SelfPath()
	if err != nil {
		return 0, err
	}
	if err := trustedFile(self, e.cfg.TrustedUID, true); err != nil {
		return 0, refuse("ccsbx-root binary: %v", err)
	}
	term := e.d.Getenv("TERM")
	if !termRe.MatchString(term) {
		term = "xterm-256color"
	}
	spec := AttachSpec{Self: self, Args: []string{"attach-exec", "--", rec.Sid, strconv.Itoa(rec.Gen), rec.PodUID},
		Env:       []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "TERM=" + term},
		LaunchUID: kc.launchUID, LaunchGID: kc.launchGID, TermUID: termUID, TermGID: termGID}
	return e.d.AttachSession(spec)
}

var attachGenRe = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// attach-exec is what the launch user's process does, never the owner's: it is refused unless it runs as
// the launch user. It checks the files again, looks the pod up by the name derived from sid and
// generation with ITS token, requires the live UID to be the one the record named, and execs kubectl on
// the pty it was given. The record is hostd's claim; the live pod is the check.
func (e *env) attachExecCmd() *cli.Command {
	return &cli.Command{
		Name:    "attach-exec",
		Summary: "internal: run by `attach` as the launch user on a pty; refuses anyone else",
		Recipes: `  Started by ccsbx-root attach itself; it does nothing for any other caller (exit 2).`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if err := e.attachExec(args); err != nil {
				return fail(stderr, "attach-exec", err)
			}
			return cli.ExitOK // not reached when Exec succeeds
		},
	}
}

func (e *env) attachExec(args []string) error {
	if len(args) != 3 || !sidMintRe.MatchString(args[0]) || !attachGenRe.MatchString(args[1]) || !podUIDRe.MatchString(args[2]) {
		return refuse("usage: attach-exec -- <sid> <gen> <pod uid>")
	}
	launchUID, _, err := e.users()
	if err != nil {
		return err
	}
	if e.d.Euid == 0 || e.d.Euid != launchUID {
		return refuse("attach-exec runs only as the launch user %s", e.cfg.LaunchUser)
	}
	gen, _ := strconv.Atoi(args[1])
	kubectl, conn, err := e.k8sFiles(launchUID)
	if err != nil {
		return err
	}
	b := k8s.NewBackend(nil, nil, k8s.DefaultConfig())
	b.Cfg.Namespace = e.cfg.K8sNamespace
	b.Kubectl, b.Kubeconfig, b.Server, b.CAFile = kubectl, e.cfg.Kubeconfig, conn.Server, conn.CAFile
	argv, err := b.AttachArgv(args[0], gen) // the pod name is derived from sid and generation here
	if err != nil {
		return refuse("%v", err)
	}
	live, err := e.d.LivePodUID(conn, e.cfg.K8sNamespace, args[0], gen)
	if err != nil {
		return fmt.Errorf("cannot read the pod of session %s: %v", args[0], err)
	}
	if live != args[2] {
		return refuse("pod_conflict: pod %s/%s is not the pod created for session %s (UID differs); delete exactly that pod and start again",
			e.cfg.K8sNamespace, k8s.PodName(args[0], gen), args[0])
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	return e.d.Exec(kubectl, argv, env)
}

// launchFile: a regular file, in a directory, both owned by the launch user or root. The file may be
// neither read nor written by group or others (it IS the launcher credential: whoever reads it can
// create and delete pods), and its directory may not be written by group or others nor entered by
// others (so the owner can neither swap it nor reach it).
func launchFile(path string, launchUID, trustedUID int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("not a clean absolute path")
	}
	if err := ownedBy(path, launchUID, trustedUID); err != nil {
		return launchOwnerErr(err)
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("readable or writable by group or others (%v): the launcher credential is for the launch user alone", st.Mode().Perm())
	}
	dir := filepath.Dir(path)
	if err := ownedBy(dir, launchUID, trustedUID); err != nil {
		return fmt.Errorf("directory: %w", launchOwnerErr(err))
	}
	if ds, err := os.Lstat(dir); err != nil || ds.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("directory is open to others: it must not be reachable by the session's owner")
	}
	return nil
}

// launchOwnerErr rewords ownedBy's owner message, which names root and the service user, for a file that
// belongs to the launch user.
func launchOwnerErr(err error) error {
	return fmt.Errorf("%s", strings.Replace(err.Error(), "neither root nor the service user", "neither root nor the launch user", 1))
}

// livePodUID is the production LivePodUID: a GET of the exact pod with the CA-pinned REST client.
func livePodUID(conn *k8s.ConnConfig, namespace, sid string, gen int) (string, error) {
	r, err := k8s.NewREST(conn)
	if err != nil {
		return "", err
	}
	cfg := k8s.DefaultConfig()
	cfg.Namespace = namespace
	return k8s.NewBackend(r, nil, cfg).LiveUID(context.Background(), sid, gen)
}
