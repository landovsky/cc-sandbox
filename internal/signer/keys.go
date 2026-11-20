// keys.go loads and creates the signer's machine key and scope file, and refuses a key directory,
// key or scope that is not what it should be. It exists because the signer is the only holder of
// this key and its scope is the whole signing policy: a loose mode, another owner or a symlink
// means someone else could have read the key or widened the policy, so the signer stops instead.
// Files are opened without following symlinks and checked on the open descriptor, so there is no
// gap between the check and the read.
package signer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/token"
)

const (
	// KeyName is the machine key inside --keys.
	KeyName = "machine.key"
	// ScopeName is the keyring-format file (one line, this key's kid) that says what the key may sign.
	ScopeName = "signer.scope"
)

// runUID is the uid the files must belong to; a variable so tests can play another user without root.
var runUID = os.Getuid

func ownerOf(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// checkDir requires the keys directory to be a real directory (not a link), owned by the running
// uid, mode exactly 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("signer: %s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("signer: key directory %s has mode %04o, want 0700", dir, fi.Mode().Perm())
	}
	if uid, ok := ownerOf(fi); ok && uid != runUID() {
		return fmt.Errorf("signer: key directory %s is owned by uid %d, want the running uid %d", dir, uid, runUID())
	}
	return nil
}

// openChecked opens path without following a symlink and returns the file with its fstat, so every
// check below is about the very file that is read.
func openChecked(path string) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("signer: %s: %w (a symlink is refused)", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("signer: %s is not a regular file", path)
	}
	return f, fi, nil
}

// readKey reads the machine key: a regular file, mode exactly 0600, owned by the running uid.
func readKey(path string) (*token.Key, error) {
	f, fi, err := openChecked(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("signer: %s has mode %04o, want 0600", path, fi.Mode().Perm())
	}
	if uid, ok := ownerOf(fi); ok && uid != runUID() {
		return nil, fmt.Errorf("signer: %s is owned by uid %d, want the running uid %d", path, uid, runUID())
	}
	b := make([]byte, 256)
	n, _ := f.Read(b)
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b[:n])))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signer: %s is not a base64 ed25519 seed", path)
	}
	return token.KeyFromSeed(seed)
}

// Init creates the machine key if it is absent and returns it. An existing key is checked, never
// replaced. The keys directory is made 0700 if missing and must be 0700 and ours either way.
func Init(dir string) (*token.Key, bool, error) {
	if dir == "" {
		return nil, false, errors.New("signer: --keys is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	if err := checkDir(dir); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, KeyName)
	if _, err := os.Lstat(path); err == nil {
		k, err := readKey(path)
		return k, false, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, false, err
	}
	// O_EXCL: never follow a planted link and never replace a key; synced so a power cut right after
	// --init can't leave an empty key whose public half was already published.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(seed) + "\n")
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, false, werr
	}
	if err := syncDir(dir); err != nil {
		return nil, false, err
	}
	k, err := readKey(path)
	return k, true, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readScope reads the scope file: a regular file owned by root or the running uid and not writable
// by group or others, in a directory with the same properties.
func readScope(path string) ([]byte, error) {
	trusted := func(fi os.FileInfo, what string) error {
		if uid, ok := ownerOf(fi); ok && uid != 0 && uid != runUID() {
			return fmt.Errorf("signer: %s is owned by uid %d, want root or uid %d", what, uid, runUID())
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("signer: %s is writable by group or others (mode %04o)", what, fi.Mode().Perm())
		}
		return nil
	}
	pd, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err := trusted(pd, "the scope file's directory"); err != nil {
		return nil, err
	}
	f, fi, err := openChecked(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := trusted(fi, path); err != nil {
		return nil, err
	}
	b := make([]byte, 64<<10)
	n, _ := f.Read(b)
	return b[:n], nil
}

// load reads the existing key (never creates one) and the scope the signer is bound to. The scope
// must carry profile→billing pairs, hold exactly one line, and be for this key.
func load(dir, scopePath string) (*token.Key, token.KeyScope, error) {
	var zero token.KeyScope
	if err := checkDir(dir); err != nil {
		return nil, zero, err
	}
	k, err := readKey(filepath.Join(dir, KeyName))
	if err != nil {
		return nil, zero, err
	}
	if scopePath == "" {
		scopePath = filepath.Join(dir, ScopeName)
	}
	b, err := readScope(scopePath)
	if err != nil {
		return nil, zero, fmt.Errorf("signer: no usable scope (it will not sign unscoped): %w", err)
	}
	ring, err := token.ParsePublicKeyring(b, scopePath)
	if err != nil {
		return nil, zero, fmt.Errorf("signer: no usable scope (it will not sign unscoped): %w", err)
	}
	sc, ok := ring[k.KID]
	if !ok || len(ring) != 1 || !bytes.Equal(sc.Pub, k.Public) {
		return nil, zero, fmt.Errorf("signer: %s must hold exactly one line, for this key (kid %s)", scopePath, k.KID)
	}
	if sc.Pairs == nil {
		return nil, zero, fmt.Errorf("signer: %s has no pairs=: the signer signs only exact profile→billing pairs", scopePath)
	}
	return k, sc, nil
}

// WriteScope writes the signer's scope line for its own key, atomically and 0600, so the machine key
// is read by this binary only (the ccsbx CLI never opens it). It returns the line written.
func WriteScope(dir, scopePath string, owners, machines []string, pairs string) (string, error) {
	if err := checkDir(dir); err != nil {
		return "", err
	}
	k, err := readKey(filepath.Join(dir, KeyName))
	if err != nil {
		return "", err
	}
	if len(owners) == 0 || len(machines) == 0 {
		return "", errors.New("signer: --owners and --machines are required")
	}
	line, err := k.KeyringLineWithPairs(owners, machines, pairs)
	if err != nil {
		return "", err
	}
	if scopePath == "" {
		scopePath = filepath.Join(dir, ScopeName)
	}
	tmp, err := os.CreateTemp(filepath.Dir(scopePath), ScopeName+".*.tmp")
	if err != nil {
		return "", err
	}
	_, werr := tmp.WriteString("# signer scope: written by ccsbx-signer --write-scope; the signer signs nothing outside it\n" + line + "\n")
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), scopePath)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", werr
	}
	return line, syncDir(filepath.Dir(scopePath))
}
