// cmd.go is `ccsbx token`: mint, verify, kid and pubkey on top of the package. It exists so tests and
// the forwarder's launcher can make tokens without writing code, and so doors get a public keyring only.

package token

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Usage is the `ccsbx token` help text.
const Usage = `ccsbx token — mint and check session tokens (U5, dev key file, direct mode)

Usage:
  ccsbx token mint   --sub S --label L --billing B --profile P --aud A [flags]
  ccsbx token verify --keyring FILE --token T --aud A [--proof P --method M --path PATH --body-file F]
  ccsbx token pubkey --key-file F --owners a,b --machines x,y --profiles P,.. --billings B,..
                                                                  (a keyring line for doors)
  ccsbx token kid    [--key-file F]

Flags: (each also CCSBX_TOKEN_<NAME>; --key-file also reads CCSBX_KEY_FILE)
  --key-file FILE   dev signing key, base64 seed, mode 0600 (mint creates it if absent;
                    pubkey and kid never do). Doors never get this file, only a keyring.
  --keyring FILE    public keys a door trusts: "<kid> <pubkey> owners=a machines=x" per line
  pubkey: --profiles (globs, e2e-*; * for any) and --billings are required: the tier the key may sign
  mint:
    --sub S         owner/machine/sid/g<gen>, e.g. tomas/hp-ubuntu/s01jd7k2m9/g1  (required)
    --label L       node label                       --billing api|subscription|stub (required)
    --profile P     profile name                      --aud A  audience, repeatable (required)
    --ttl D         lifetime, at most 12h (default 12h)
    --repos R,..  --routes R,..  --branch B  --placed-by W   optional claims
    --fwd-secrets   print the JSON a forwarder reads on stdin ({"token","key"}) with a fresh
                    per-generation key, instead of the bare token
  verify:
    --token T --aud A --proof P --method M --path PATH --body-file F   without --proof only the
                    signature, key scope, expiry and audience are checked, and the output says so

Recipes:
  start a forwarder for a test session:
    ccsbx token mint --sub tomas/hp-ubuntu/sX/g1 --label t --billing stub --profile e2e-test \
       --aud ccsbx-stub --fwd-secrets | ccsbx fwd --listen unix:/run/user/$UID/fwd.sock --door stub=http://127.0.0.1:8080
  give a door its trust root:  ccsbx token pubkey --key-file $CCSBX_KEY_FILE --owners tomas \
                                 --machines hp-ubuntu > keyring
Exit codes: 0 ok, 1 failure, 2 token refused, 64 usage.
`

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_TOKEN_" + name); v != "" {
		return v
	}
	return def
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

func csv(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// Main is `ccsbx token`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, Usage)
		return 64
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" || a == "help" {
			fmt.Fprint(stdout, Usage)
			return 0
		}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("key-file", envOr("KEY_FILE", os.Getenv("CCSBX_KEY_FILE")), "")
	var aud list
	fs.Var(&aud, "aud", "")
	subj := fs.String("sub", envOr("SUB", ""), "")
	label := fs.String("label", envOr("LABEL", ""), "")
	billing := fs.String("billing", envOr("BILLING", ""), "")
	profile := fs.String("profile", envOr("PROFILE", ""), "")
	ttl := fs.Duration("ttl", MaxLifetime, "")
	repos := fs.String("repos", "", "")
	routes := fs.String("routes", "", "")
	branch := fs.String("branch", "", "")
	placedBy := fs.String("placed-by", "", "")
	fwdSecrets := fs.Bool("fwd-secrets", false, "")
	keyring := fs.String("keyring", envOr("KEYRING", ""), "")
	owners := fs.String("owners", "", "")
	machines := fs.String("machines", "", "")
	profiles := fs.String("profiles", envOr("PROFILES", ""), "")
	billings := fs.String("billings", envOr("BILLINGS", ""), "")
	bodyFile := fs.String("body-file", "", "")
	tok := fs.String("token", "", "")
	proof := fs.String("proof", "", "")
	method := fs.String("method", "POST", "")
	path := fs.String("path", "/", "")
	if err := fs.Parse(rest); err != nil {
		return 64
	}
	switch sub {
	case "kid":
		k, err := LoadKeyFile(*keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "token:", err)
			return 1
		}
		fmt.Fprintln(stdout, k.KID)
		return 0

	case "pubkey":
		k, err := LoadKeyFile(*keyFile)
		if err != nil || *owners == "" || *machines == "" || *profiles == "" || *billings == "" {
			if err != nil {
				fmt.Fprintln(stderr, "token:", err)
			} else {
				fmt.Fprintln(stderr, "token pubkey: --owners, --machines, --profiles and --billings are required (a key with no scope or tier is refused)")
			}
			return 64
		}
		fmt.Fprintln(stdout, k.KeyringLineScoped(csv(*owners), csv(*machines), csv(*profiles), csv(*billings)))
		return 0

	case "mint":
		if *subj == "" || *billing == "" || *profile == "" || len(aud) == 0 {
			fmt.Fprintln(stderr, "token mint: --sub, --billing, --profile and --aud are required (see --help)")
			return 64
		}
		k, err := LoadOrCreateKeyFile(*keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "token:", err)
			return 1
		}
		gk, err := NewGenKey()
		if err != nil {
			fmt.Fprintln(stderr, "token:", err)
			return 1
		}
		now := time.Now()
		t, err := Mint(k, Claims{Sub: *subj, Label: *label, Billing: *billing, Profile: *profile, Aud: aud,
			Routes: csv(*routes), Repos: csv(*repos), Branch: *branch, PlacedBy: *placedBy,
			Cnf: CnfOf(gk.Public), Exp: now.Add(*ttl).Unix()}, now)
		if err != nil {
			fmt.Fprintln(stderr, "token mint:", err)
			return 2
		}
		if !*fwdSecrets {
			fmt.Fprintln(stdout, t) // the generation key is discarded: this token can never be proven
			return 0
		}
		json.NewEncoder(stdout).Encode(map[string]string{"token": t, "key": base64.StdEncoding.EncodeToString(gk.Private.Seed())})
		return 0

	case "verify":
		ring, err := LoadPublicKeyring(*keyring)
		if err != nil || *tok == "" || len(aud) == 0 {
			if err != nil {
				fmt.Fprintln(stderr, "token:", err)
			} else {
				fmt.Fprintln(stderr, "token verify: --token and --aud are required")
			}
			return 64
		}
		v := NewVerifier(ring, aud[0])
		var body []byte
		if *bodyFile != "" {
			if body, err = os.ReadFile(*bodyFile); err != nil {
				fmt.Fprintln(stderr, "token:", err)
				return 1
			}
		}
		var c *Claims
		note := "signature, key scope, expiry and audience only; no proof given"
		if *proof == "" {
			c, err = v.checkNoProof(*tok, time.Now())
		} else {
			c, err = v.Verify(*tok, *proof, *method, *path, body, time.Now())
			note = "full check incl. proof of possession"
		}
		if err != nil {
			fmt.Fprintln(stderr, "token verify: refused:", err)
			return 2
		}
		json.NewEncoder(stdout).Encode(map[string]any{"ok": true, "checked": note, "claims": c})
		return 0
	}
	fmt.Fprintf(stderr, "token: unknown subcommand %q (see --help)\n", sub)
	return 64
}

// checkNoProof is Verify without the proof and state steps, for the CLI only.
func (v *Verifier) checkNoProof(tok string, now time.Time) (*Claims, error) {
	ring := v.Ring()
	c, err := parse(tok, ring)
	if err != nil {
		return nil, err
	}
	owner, machine, _, _, err := c.SubParts()
	if err != nil {
		return nil, err
	}
	if !ring[c.kid].allows(owner, machine) {
		return nil, ErrKeyScope
	}
	if now.Unix() >= c.Exp {
		return nil, ErrExpired
	}
	for _, a := range c.Aud {
		if a == v.aud {
			return c, nil
		}
	}
	return nil, ErrAudience
}
