// client.go is the signer's client half: ask the socket to sign claims, and build the secrets a
// pod's sidecar is loaded with. It exists for the pod launcher (ccsbx-keys), which generates the
// generation key itself, has the claims signed with the key's cnf, and passes {token, key} to the
// sidecar on stdin: the key never crosses the signer's socket and hostd never sees either half.
package signer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Client talks to a signer socket.
type Client struct {
	Socket  string
	Timeout time.Duration // default 5 s
}

// Sign sends the claims and returns the token. A refusal comes back as an error with the signer's text.
func (c Client) Sign(ctx context.Context, cl token.Claims) (string, error) {
	to := c.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	var d net.Dialer
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return "", fmt.Errorf("signer: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(to))
	if err := json.NewEncoder(conn).Encode(cl); err != nil {
		return "", err
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return "", fmt.Errorf("signer: no reply: %v", err)
	}
	if r.Error != "" {
		return "", fmt.Errorf("signer refused: %s", r.Error)
	}
	if r.Token == "" {
		return "", errors.New("signer: empty token")
	}
	return r.Token, nil
}

// MintFwdSecrets makes a fresh generation key, has claims (with cnf set to its public half) signed,
// and returns the JSON the sidecar's load reads: {"token":..., "key": base64 of the 32-byte seed}.
// It is what `ccsbx token mint --fwd-secrets` prints, built in-process.
func MintFwdSecrets(ctx context.Context, sign func(context.Context, token.Claims) (string, error), cl token.Claims) ([]byte, error) {
	gk, err := token.NewGenKey()
	if err != nil {
		return nil, err
	}
	cl.Cnf = token.CnfOf(gk.Public)
	t, err := sign(ctx, cl)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"token": t, "key": base64.StdEncoding.EncodeToString(gk.Private.Seed())})
}
