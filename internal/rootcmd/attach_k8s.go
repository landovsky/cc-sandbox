// `ccsbx-root attach <sid>` for a session that runs as a pod (F5): the owner's terminal is joined to
// the fence's tmux with kubectl exec. kubectl runs as the keys user (ccsbx-keys), who alone can read
// the launcher kubeconfig and token; the owner never can. Because the owner's session record, files
// and environment are all hostile input, everything kubectl will read is checked here first, the
// server and the CA come from a root-owned file and are put on kubectl's command line, and the
// pod's live UID must equal the one recorded when we created it before kubectl is exec'd.
package rootcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// k8sCtx is everything a pod verb needs once the record and the files have been checked: where
// kubectl is, who to become, and the cluster connection (server and CA from the root-owned file).
type k8sCtx struct {
	kubectl          string
	keysUID, keysGID int
	conn             *k8s.ConnConfig
}

// k8sSetup is the part every pod verb shares. Nothing here runs kubectl or reads the token: it
// checks, as root, that everything the keys user is about to read is something the owner cannot
// change, and that the keys user is not the caller, the record's owner or the service user.
func (e *env) k8sSetup(w who, rec *start.Record) (*k8sCtx, error) {
	kubectl, err := e.cfg.binary(e.cfg.Kubectl)
	if err != nil {
		return nil, err
	}
	keysUID, keysGID, err := e.d.LookupUser(e.cfg.KeysUser)
	if err != nil {
		return nil, refuse("keys user %s: %v", e.cfg.KeysUser, err)
	}
	switch {
	case keysUID == w.uid:
		return nil, refuse("keys user %s is the caller: the caller could rewrite what kubectl reads", e.cfg.KeysUser)
	case keysUID == rec.Uid:
		return nil, refuse("keys user %s is the session's owner: the owner could rewrite what kubectl reads", e.cfg.KeysUser)
	case keysUID == w.service:
		return nil, refuse("keys user %s is the service user: hostd would hold the launcher credential", e.cfg.KeysUser)
	case keysGID == 0 || keysGID == w.serviceGid:
		return nil, refuse("keys user %s has primary group %d, which is root's or the service group: it would keep that group after the drop", e.cfg.KeysUser, keysGID)
	}
	if err := trustedFile(e.cfg.K8sConn, e.cfg.TrustedUID, true); err != nil {
		return nil, refuse("k8s connection file: %v", err)
	}
	conn, err := k8s.LoadConnConfig(e.cfg.K8sConn)
	if err != nil {
		return nil, refuse("k8s connection file: %v", err)
	}
	if err := trustedFile(conn.CAFile, e.cfg.TrustedUID, true); err != nil {
		return nil, refuse("cluster CA: %v", err)
	}
	for name, p := range map[string]string{"kubeconfig": e.cfg.Kubeconfig, "launcher token": conn.TokenFile} {
		if err := keysFile(p, keysUID, e.cfg.TrustedUID); err != nil {
			return nil, refuse("%s %s: %v", name, p, err)
		}
	}
	return &k8sCtx{kubectl: kubectl, keysUID: keysUID, keysGID: keysGID, conn: conn}, nil
}

func (e *env) attachK8s(w who, rec *start.Record) error {
	// Only the owner (or root by login) gets a terminal in a pod. The service user (hostd) may start and
	// stop pod sessions but must not hold kubectl exec into every fence with the launcher credential.
	if w.uid != rec.Uid && w.uid != 0 {
		return refuse("no such session %q (or it is not yours)", rec.Sid)
	}
	if rec.PodUID == "" {
		return refuse("session %s has no pod UID recorded, so its pod cannot be told from another under the same name", rec.Sid)
	}
	kc, err := e.k8sSetup(w, rec)
	if err != nil {
		return err
	}
	b := k8s.NewBackend(nil, nil, k8s.DefaultConfig())
	b.Cfg.Namespace = e.cfg.K8sNamespace
	b.Kubectl, b.Kubeconfig, b.Server, b.CAFile = kc.kubectl, e.cfg.Kubeconfig, kc.conn.Server, kc.conn.CAFile
	argv, err := b.AttachArgv(rec.Sid, rec.Gen) // the pod name is derived from sid and generation here
	if err != nil {
		return refuse("%v", err)
	}
	term := e.d.Getenv("TERM")
	if !termRe.MatchString(term) {
		term = "xterm-256color"
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "TERM=" + term}
	tmux, err := e.cfg.binary(e.cfg.Tmux)
	if err != nil {
		return err
	}
	sock, err := e.keysSocket(kc, rec.Sid)
	if err != nil {
		return err
	}
	if err := e.d.DropPrivs(kc.keysUID, kc.keysGID); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.KeysUser, err)
	}
	// The record is hostd's (it writes pod_uid), so it is believed only as far as it can be checked:
	// the pod is looked up again by the name derived from sid and generation, with the keys user's
	// token, and its live UID must be the recorded one. A pod someone else put under that name has
	// another UID and never gets the owner's terminal.
	live, err := e.d.LivePodUID(kc.conn, e.cfg.K8sNamespace, rec.Sid, rec.Gen)
	if err != nil {
		return fmt.Errorf("cannot read the pod of session %s: %v", rec.Sid, err)
	}
	if live != rec.PodUID {
		return refuse("pod_conflict: pod %s/%s is not the pod created for session %s (UID differs); delete exactly that pod and start again",
			e.cfg.K8sNamespace, k8s.PodName(rec.Sid, rec.Gen), rec.Sid)
	}
	// The pod's tmux runs as the agent's own uid, so the bytes it sends are the agent's: OSC 52 clipboard
	// writes, passthrough sequences, queries whose answers land in the owner's shell. They must not reach
	// the owner's terminal raw. So kubectl runs as the pane of a host-side tmux server owned by the keys
	// user, which parses and re-renders them (passthrough and clipboard off, no prefix, no bindings, no
	// mouse, no status line), and the owner's terminal attaches to THAT server.
	hostArgv := []string{"tmux", "-S", sock, "-f", "/dev/null", "start-server", ";",
		"set-option", "-g", "allow-passthrough", "off", ";",
		"set-option", "-g", "set-clipboard", "off", ";",
		"set-option", "-g", "prefix", "None", ";",
		"set-option", "-g", "prefix2", "None", ";",
		"set-option", "-g", "mouse", "off", ";",
		"set-option", "-g", "status", "off", ";",
		"unbind-key", "-a", ";",
		"unbind-key", "-a", "-T", "root", ";",
		"new-session", "-A", "-s", rec.Sid, "--"}
	return e.d.Exec(tmux, append(hostArgv, argv...), env)
}

// keysSocket makes sure the keys user's run directory exists (0700, theirs, not a link) and returns
// the host tmux socket path for one session. Root makes it; the keys user's tmux creates the socket.
func (e *env) keysSocket(kc *k8sCtx, sid string) (string, error) {
	dir := e.cfg.KeysRunDir
	if st, err := os.Lstat(dir); os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", fmt.Errorf("keys run directory: %v", err)
		}
		chown := e.d.Chown
		if chown == nil {
			chown = os.Lchown
		}
		if err := chown(dir, kc.keysUID, kc.keysGID); err != nil {
			return "", fmt.Errorf("keys run directory: %v", err)
		}
	} else if err != nil {
		return "", err
	} else if st.Mode()&os.ModeSymlink != 0 {
		return "", refuse("keys run directory %s is a symlink", dir)
	}
	if err := ownedBy(dir, kc.keysUID, e.cfg.TrustedUID); err != nil {
		return "", refuse("keys run directory %s: %v", dir, keysOwnerErr(err))
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return "", refuse("keys run directory %s must be a directory only the keys user can enter", dir)
	}
	return filepath.Join(dir, sid+".sock"), nil
}

// keysFile: a regular file, in a directory, both owned by the keys user or root. The file may be
// neither read nor written by group or others (it IS the launcher credential: whoever reads it can
// create and delete pods), and its directory may not be written by group or others nor entered by
// others (so the owner can neither swap it nor reach it).
func keysFile(path string, keysUID, trustedUID int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("not a clean absolute path")
	}
	if err := ownedBy(path, keysUID, trustedUID); err != nil {
		return keysOwnerErr(err)
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("readable or writable by group or others (%v): the launcher credential is for the keys user alone", st.Mode().Perm())
	}
	dir := filepath.Dir(path)
	if err := ownedBy(dir, keysUID, trustedUID); err != nil {
		return fmt.Errorf("directory: %w", keysOwnerErr(err))
	}
	if ds, err := os.Lstat(dir); err != nil || ds.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("directory is open to others: it must not be reachable by the session's owner")
	}
	return nil
}

// keysOwnerErr rewords ownedBy's owner message, which names root and the service user, for a file that
// belongs to the keys user.
func keysOwnerErr(err error) error {
	return fmt.Errorf("%s", strings.Replace(err.Error(), "neither root nor the service user", "neither root nor the keys user", 1))
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
