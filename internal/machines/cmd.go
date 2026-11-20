// cmd.go is `ccsbx machines`: check the trusted-key list, print it as keyring lines, or render the
// ConfigMap. It exists so a change to config/machines/ is checked and reviewed as the exact bytes
// the cluster would get, and so nothing here needs (or has) cluster access: Render prints, it never applies.
package machines

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/token"
)

const usage = `ccsbx machines: the trusted-key list (plan M9), config/machines/<machine>.json

Usage:
  ccsbx machines check [DIR]             read every file, refuse the first problem (exit 1), print one line per key
  ccsbx machines ring [DIR]              the same list as keyring lines (for ccsbx stub --keyring, ccsbx token verify)
  ccsbx machines render [DIR] [--name N] [--namespace NS]
                                         the ConfigMap YAML on stdout, to review; this command never applies it
  ccsbx machines --help                  this text; nothing runs

DIR defaults to $CCSBX_MACHINES_DIR, else config/machines. Each file: machine, owners, public_keys,
may_sign_machines, max_profile, max_billing, pairs (see internal/machines/machines.go).

Flags:
  --name N        ConfigMap name for render (default ccsbx-machines; CCSBX_MACHINES_NAME)
  --namespace NS  namespace for render (default: none, kustomize sets it; CCSBX_MACHINES_NAMESPACE)

Recipes:
  roll a key      add the new key to public_keys, apply, roll the signer, then remove the old key
  remove a key    delete it from public_keys (or the file for a machine); doors refuse it within minutes
  check a change  ccsbx machines check && ccsbx machines render | less
Exit: 0 ok, 1 the list is bad, 64 usage.
`

// Main runs the command.
func Main(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		io.WriteString(stdout, usage)
		return 0
	}
	sub := args[0]
	fs := flag.NewFlagSet("machines "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", orDefault(getenv("CCSBX_MACHINES_NAME"), "ccsbx-machines"), "")
	ns := fs.String("namespace", getenv("CCSBX_MACHINES_NAMESPACE"), "")
	// flags may follow DIR
	var rest []string
	for a := args[1:]; len(a) > 0; {
		if err := fs.Parse(a); err != nil {
			return 64
		}
		if fs.NArg() > 0 {
			rest = append(rest, fs.Arg(0))
			a = fs.Args()[1:]
		} else {
			break
		}
	}
	if len(rest) > 1 {
		fmt.Fprintln(stderr, "ccsbx machines: at most one DIR")
		return 64
	}
	dir := orDefault(getenv("CCSBX_MACHINES_DIR"), "config/machines")
	if len(rest) == 1 {
		dir = rest[0]
	}
	if sub == "render" {
		for _, v := range []string{*name, *ns} {
			if v != "" && !token.ValidName(v) { // goes into YAML unquoted
				fmt.Fprintf(stderr, "ccsbx machines: %q is not a plain name\n", v)
				return 64
			}
		}
	}
	es, err := LoadDir(dir)
	if err == nil {
		_, err = Ring(es)
	}
	switch sub {
	case "check", "ring", "render":
	default:
		fmt.Fprintf(stderr, "ccsbx machines: unknown command %q (ccsbx machines --help)\n", sub)
		return 64
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx machines:", err)
		return 1
	}
	switch sub {
	case "check":
		for _, e := range es {
			fmt.Fprintf(stdout, "%s: %d key(s), owners=%s signs=%s profiles=%s billings=%s\n", e.Machine, len(e.PublicKeys),
				strings.Join(e.Owners, ","), strings.Join(e.MaySignMachines, ","), strings.Join(e.MaxProfile, ","), strings.Join(e.MaxBilling, ","))
		}
	case "ring":
		ls, _ := Lines(es)
		for _, l := range ls {
			fmt.Fprintln(stdout, l)
		}
	case "render":
		fmt.Fprint(stdout, Render(es, *name, *ns))
	}
	return 0
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Render is the ConfigMap: one data key per machine file, the JSON re-written canonically from the
// parsed entry (so what is applied is what was validated, and comments or odd spacing can't differ).
func Render(es []*Entry, name, namespace string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n")
	if namespace != "" {
		b.WriteString("  namespace: " + namespace + "\n")
	}
	b.WriteString("data:\n")
	sorted := append([]*Entry(nil), es...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Machine < sorted[j].Machine })
	for _, e := range sorted {
		b.WriteString("  " + e.Machine + ".json: |\n")
		for _, l := range strings.Split(strings.TrimRight(canonical(e), "\n"), "\n") {
			b.WriteString("    " + l + "\n")
		}
	}
	return b.String()
}
