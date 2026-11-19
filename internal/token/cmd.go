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
  ccsbx token verify --token T [--proof P --method M --path PATH] --aud A [flags]
  ccsbx token kid    [--key-file F]

Flags: (each also CCSBX_TOKEN_<NAME>; --key-file also reads CCSBX_KEY_FILE)
  --key-file FILE   dev signing key, base64 seed, mode 0600 (mint creates it if absent;
                    verify and kid never do)
  mint:
    --sub S         owner/machine/sid/g<gen>, e.g. tomas/hp-ubuntu/s01jd7k2m9/g1  (required)
    --label L       node label                       --billing api|subscription|stub (required)
    --profile P     profile name                      --aud A  audience, repeatable (required)
    --ttl D         lifetime, at most 12h (default 12h)
    --repos R,..  --routes R,..  --branch B  --placed-by W   optional claims
    --fwd-secrets   print the JSON a forwarder reads on stdin ({"token","key"}) with a fresh
                    per-generation key, instead of the bare token
  verify:
    --token T --aud A --proof P --method M --path PATH   without --proof only the signature,
                    expiry and audience are checked, and the output says so

Recipes:
  start a forwarder for a test session:
    ccsbx token mint --sub tomas/hp-ubuntu/sX/g1 --label t --billing stub --profile e2e-test \
       --aud ccsbx-model --fwd-secrets | ccsbx fwd --listen 127.0.0.1:7481 --door model=http://127.0.0.1:8080
  which key signs?   ccsbx token kid
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
		k, err := LoadKeyFile(*keyFile)
		if err != nil || *tok == "" || len(aud) == 0 {
			if err != nil {
				fmt.Fprintln(stderr, "token:", err)
			} else {
				fmt.Fprintln(stderr, "token verify: --token and --aud are required")
			}
			return 64
		}
		v := NewVerifier(Keyring{k.KID: k.Public}, aud[0])
		var c *Claims
		note := "signature, expiry and audience only; no proof given"
		if *proof == "" {
			c, err = v.checkNoProof(*tok, time.Now())
		} else {
			c, err = v.Verify(*tok, *proof, *method, *path, time.Now())
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
	c, err := parse(tok, v.ring)
	if err != nil {
		return nil, err
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
