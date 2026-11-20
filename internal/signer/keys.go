// keys.go loads and creates the signer's machine key and its scope file, and refuses a key that is
// not what it should be. It exists because the signer is the only holder of this key: a key file
// with the wrong owner or a loose mode means someone else could have read or replaced it, and the
// signer must say so and stop rather than sign with it.
package signer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/token"
)

const (
	// KeyName is the machine key inside --keys.
	KeyName = "machine.key"
	// ScopeName is the keyring-format file (one line, this key's kid) that says what the key may sign.
	ScopeName = "signer.scope"
)

// checkKeyFile refuses a key file that is not a regular file, not owned by the running uid, or
// readable by anyone else.
func checkKeyFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("signer: %s is not a regular file", path)
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("signer: %s has mode %04o, want 0600", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("signer: %s is owned by uid %d, want the running uid %d", path, st.Uid, os.Getuid())
	}
	return nil
}

// Init creates the machine key if it is absent and returns it. An existing key is checked, never
// replaced: with a wrong owner or mode it is refused. The keys directory is made 0700 if missing.
func Init(dir string) (*token.Key, bool, error) {
	if dir == "" {
		return nil, false, errors.New("signer: --keys is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, KeyName)
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if !created {
		if err := checkKeyFile(path); err != nil {
			return nil, false, err
		}
	}
	k, err := token.LoadOrCreateKeyFile(path)
	if err != nil {
		return nil, false, err
	}
	if err := checkKeyFile(path); err != nil {
		return nil, false, err
	}
	return k, created, nil
}

// load reads the existing key (never creates one) and the scope the signer is bound to.
func load(dir, scopePath string) (*token.Key, token.KeyScope, error) {
	var zero token.KeyScope
	path := filepath.Join(dir, KeyName)
	if err := checkKeyFile(path); err != nil {
		return nil, zero, err
	}
	k, err := token.LoadKeyFile(path)
	if err != nil {
		return nil, zero, err
	}
	if scopePath == "" {
		scopePath = filepath.Join(dir, ScopeName)
	}
	ring, err := token.LoadPublicKeyring(scopePath) // requires owners, machines, profiles and billings
	if err != nil {
		return nil, zero, fmt.Errorf("signer: no usable scope (it will not sign unscoped): %w", err)
	}
	sc, ok := ring[k.KID]
	if !ok || len(ring) != 1 {
		return nil, zero, fmt.Errorf("signer: %s must hold exactly one line, for this key (kid %s)", scopePath, k.KID)
	}
	return k, sc, nil
}
