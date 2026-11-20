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

	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/start"
)

func (e *env) attachK8s(w who, rec *start.Record) error {
	if rec.PodUID == "" {
		return refuse("session %s has no pod UID recorded, so its pod cannot be told from another under the same name", rec.Sid)
	}
	kubectl, err := e.cfg.binary(e.cfg.Kubectl)
	if err != nil {
		return err
	}
	keysUID, keysGID, err := e.d.LookupUser(e.cfg.KeysUser)
	if err != nil {
		return refuse("keys user %s: %v", e.cfg.KeysUser, err)
	}
	if keysUID == w.uid || keysUID == w.service || keysUID == rec.Uid {
		return refuse("keys user %s must be neither the caller nor the service user", e.cfg.KeysUser)
	}
	// what kubectl reads, as the keys user, must not be something the owner can change
	if err := trustedFile(e.cfg.K8sConn, e.cfg.TrustedUID, true); err != nil {
		return refuse("k8s connection file: %v", err)
	}
	conn, err := k8s.LoadConnConfig(e.cfg.K8sConn)
	if err != nil {
		return refuse("k8s connection file: %v", err)
	}
	if err := trustedFile(conn.CAFile, e.cfg.TrustedUID, true); err != nil {
		return refuse("cluster CA: %v", err)
	}
	for name, p := range map[string]string{"kubeconfig": e.cfg.Kubeconfig, "launcher token": conn.TokenFile} {
		if err := keysFile(p, keysUID, e.cfg.TrustedUID); err != nil {
			return refuse("%s %s: %v", name, p, err)
		}
	}
	b := k8s.NewBackend(nil, nil, k8s.DefaultConfig())
	b.Cfg.Namespace = e.cfg.K8sNamespace
	b.Kubectl, b.Kubeconfig, b.Server, b.CAFile = kubectl, e.cfg.Kubeconfig, conn.Server, conn.CAFile
	argv, err := b.AttachArgv(rec.Sid, rec.Gen) // the pod name is derived from sid and generation here
	if err != nil {
		return refuse("%v", err)
	}
	term := e.d.Getenv("TERM")
	if !termRe.MatchString(term) {
		term = "xterm-256color"
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "TERM=" + term}
	if err := e.d.DropPrivs(keysUID, keysGID); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.KeysUser, err)
	}
	// the live pod must be the one we created; this is read with the keys user's token, after the drop
	live, err := e.d.LivePodUID(conn, e.cfg.K8sNamespace, rec.Sid, rec.Gen)
	if err != nil {
		return fmt.Errorf("cannot read the pod of session %s: %v", rec.Sid, err)
	}
	if live != rec.PodUID {
		return refuse("pod_conflict: pod %s/%s is not the pod created for session %s (UID differs); delete exactly that pod and start again",
			e.cfg.K8sNamespace, k8s.PodName(rec.Sid, rec.Gen), rec.Sid)
	}
	return e.d.Exec(kubectl, argv[:], env)
}

// keysFile: a regular file, in a directory, both owned by the keys user or root and writable by
// neither group nor others (so the owner cannot swap what kubectl reads).
func keysFile(path string, keysUID, trustedUID int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("not a clean absolute path")
	}
	if err := ownedBy(path, keysUID, trustedUID); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if err := ownedBy(filepath.Dir(path), keysUID, trustedUID); err != nil {
		return fmt.Errorf("directory: %w", err)
	}
	return nil
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
