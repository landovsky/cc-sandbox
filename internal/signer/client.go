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
	r, err := c.ask(ctx, cl)
	if err != nil {
		return "", err
	}
	if r.Token == "" {
		return "", errors.New("signer: empty token")
	}
	return r.Token, nil
}

// SignRevoke asks for the header and the exact body of a revoke of generation gen of a session.
func (c Client) SignRevoke(ctx context.Context, b token.RevokeBody) (header string, body []byte, err error) {
	r, err := c.ask(ctx, map[string]any{"revoke": b})
	if err != nil {
		return "", nil, err
	}
	if r.Revoke == "" || r.Body == "" {
		return "", nil, errors.New("signer: empty revoke")
	}
	return r.Revoke, []byte(r.Body), nil
}

func (c Client) ask(ctx context.Context, req any) (*reply, error) {
	to := c.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	var d net.Dialer
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("signer: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(to))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return nil, fmt.Errorf("signer: no reply: %v", err)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("signer refused: %s", r.Error)
	}
	return &r, nil
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

// MintPodSecrets is MintFwdSecrets plus a relay credential: the generation key is made here, the token
// signed with its public half as cnf, and register(pub) (the hub's relay principal for that same public
// key) supplies the third secret. The payload is {"token","key","relay_token"}, the one thing the pod's
// sidecar is loaded with; none of the three leaves this process except on that stdin.
func MintPodSecrets(ctx context.Context, sign func(context.Context, token.Claims) (string, error), cl token.Claims, register func(pub string) (string, error)) ([]byte, error) {
	gk, err := token.NewGenKey()
	if err != nil {
		return nil, err
	}
	pub := token.CnfOf(gk.Public)
	cl.Cnf = pub
	t, err := sign(ctx, cl)
	if err != nil {
		return nil, err
	}
	rt, err := register(pub) // cnf equality with the relay's key holds by construction
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"token": t, "key": base64.StdEncoding.EncodeToString(gk.Private.Seed()), "relay_token": rt})
}
