// Package tmuxsock turns the --tmux-socket setting into tmux's own flag. It exists because three
// programs (hostd's delivery, the pane launcher, `ccsbx attach`) must address the same tmux server,
// and once hostd runs as a service uid that server is reached through a shared socket PATH, not a
// per-user -L name: a bare name means `tmux -L name`, an absolute path means `tmux -S path`.
package tmuxsock

import "path/filepath"

// Args is the tmux flag pair that selects the server: ["-S", path] for an absolute path,
// ["-L", name] otherwise.
func Args(sock string) []string {
	if filepath.IsAbs(sock) {
		return []string{"-S", sock}
	}
	return []string{"-L", sock}
}

// IsPath reports whether sock is a socket path (shared between uids) rather than a -L name.
func IsPath(sock string) bool { return filepath.IsAbs(sock) }
