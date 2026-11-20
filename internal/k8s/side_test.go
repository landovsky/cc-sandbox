// Tests for the sidecar's command line: exact flags from the record, nothing the fence can write, every
// value checked because the pod spec is read by the cluster, and the contract holds the command to
// `ccsbx side serve`.
package k8s

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func goodSide() SideInfo {
	return SideInfo{Sid: "s0abcdefgh", Gen: 2, Label: "cc-sandbox:x", FullID: "tomas/future-two/s0abcdefgh/g2", Machine: "future-two",
		Scope: json.RawMessage(`{ "manager": {"node": "cc-sandbox:#6b3s", "machine": "hp-ubuntu"}, "peers": [] }`), HubURL: "http://hub-egress.cc-sandbox.svc:4011",
		Doors: []string{"model=http://ccsp-model.cc-sandbox-doors.svc:8080"}, GitRepo: "landovsky/cc-sandbox", GitBase: "main", GitBranch: "work/s0abcdefgh", GitMaxBytes: 1 << 30}
}

func TestSideArgvIsExactlyWhatTheSidecarsPathsAndRecordSay(t *testing.T) {
	got, err := SideArgv(goodSide())
	if err != nil {
		t.Fatal(err)
	}
	want := "/usr/local/bin/ccsbx side serve --listen 127.0.0.1:8080 --control /run/ccsbx/side/ctl/ctl.sock --state /run/ccsbx/side/state --journal-dir /run/ccsbx/journal/s " +
		"--out /run/ccsbx/out --in /run/ccsbx/in --fence-uid 10001 --sid s0abcdefgh --gen 2 --label cc-sandbox:x --full-id tomas/future-two/s0abcdefgh/g2 " +
		`--machine future-two --scope {"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":[]} ` +
		"--git-repo landovsky/cc-sandbox --git-url http://127.0.0.1:8080/egress/git --git-base main --git-branch work/s0abcdefgh --git-dir /run/ccsbx/side/git/g --git-max-bytes 1073741824 " +
		"--hub-url http://hub-egress.cc-sandbox.svc:4011 --door model=http://ccsp-model.cc-sandbox-doors.svc:8080"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
	// no hub URL until the egress Service exists: the flag is left out, not empty
	i := goodSide()
	i.HubURL, i.Doors, i.GitBase, i.GitMaxBytes = "", nil, "", 0
	got, _ = SideArgv(i)
	if strings.Contains(strings.Join(got, " "), "--hub-url") || strings.Contains(strings.Join(got, " "), "--door") {
		t.Errorf("%v", got)
	}
	// and what the sidecar must never be handed
	for _, a := range got {
		for _, secret := range []string{"token", "seed", "secret", "key"} {
			if strings.Contains(strings.ToLower(a), secret) && a != "--fence-uid" {
				t.Errorf("argument %q looks like a secret", a)
			}
		}
	}
}

func TestSideArgvRefusesWhatItCannotVouchFor(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", maxScope) + `"}`
	for why, mod := range map[string]func(*SideInfo){
		"a bad sid":                      func(i *SideInfo) { i.Sid = "../x" },
		"generation 0":                   func(i *SideInfo) { i.Gen = 0 },
		"no label":                       func(i *SideInfo) { i.Label = "" },
		"a label with a newline":         func(i *SideInfo) { i.Label = "a\nb" },
		"a label over 200":               func(i *SideInfo) { i.Label = strings.Repeat("l", 201) },
		"a full id of another session":   func(i *SideInfo) { i.FullID = "tomas/future-two/s0zzzzzzzz/g2" },
		"a full id of another gen":       func(i *SideInfo) { i.FullID = "tomas/future-two/s0abcdefgh/g3" },
		"a malformed full id":            func(i *SideInfo) { i.FullID = "s0abcdefgh/g2" },
		"a bad machine":                  func(i *SideInfo) { i.Machine = "a/b" },
		"no scope":                       func(i *SideInfo) { i.Scope = nil },
		"a scope that is not an object":  func(i *SideInfo) { i.Scope = json.RawMessage(`["x"]`) },
		"a scope that is not JSON":       func(i *SideInfo) { i.Scope = json.RawMessage(`{nope`) },
		"a scope over the limit":         func(i *SideInfo) { i.Scope = json.RawMessage(big) },
		"a negative git max bytes":       func(i *SideInfo) { i.GitMaxBytes = -1 },
		"no repository":                  func(i *SideInfo) { i.GitRepo = "" },
		"a repository that is a URL":     func(i *SideInfo) { i.GitRepo = "https://x/y" },
		"a repository with a dot dot":    func(i *SideInfo) { i.GitRepo = "../x" },
		"a base that is an option":       func(i *SideInfo) { i.GitBase = "--upload-pack=x" },
		"a branch with dot dot":          func(i *SideInfo) { i.GitBranch = "a..b" },
		"a branch ending in .lock":       func(i *SideInfo) { i.GitBranch = "x.lock" },
		"a branch with a space":          func(i *SideInfo) { i.GitBranch = "a b" },
		"a hub URL with credentials":     func(i *SideInfo) { i.HubURL = "http://u:p@hub:4011" },
		"a hub URL with a path":          func(i *SideInfo) { i.HubURL = "http://hub:4011/x" },
		"a hub URL that is ftp":          func(i *SideInfo) { i.HubURL = "ftp://hub" },
		"a door with no name":            func(i *SideInfo) { i.Doors = []string{"http://x"} },
		"a door with credentials":        func(i *SideInfo) { i.Doors = []string{"model=http://u:p@x"} },
		"a door with an upper-case name": func(i *SideInfo) { i.Doors = []string{"Model=http://x"} },
	} {
		i := goodSide()
		mod(&i)
		if a, err := SideArgv(i); err == nil {
			t.Errorf("%s was accepted: %v", why, a)
		}
	}
}

func TestThePodCarriesTheSidecarCommandAndTheContractHoldsItToServe(t *testing.T) {
	sd := func(p *Pod) *Container { return &p.Spec.InitContainers[0] }
	argv, err := SideArgv(goodSide())
	if err != nil {
		t.Fatal(err)
	}
	s := goodSpec()
	s.SideCmd = argv
	p, err := PodFor(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Spec.InitContainers[0].Command, " ") != strings.Join(argv, " ") {
		t.Errorf("the sidecar's command is not the one built: %v", p.Spec.InitContainers[0].Command)
	}
	for why, mod := range map[string]func(*Pod){
		"a sidecar that runs a shell":   func(p *Pod) { sd(p).Command = []string{"/bin/sh", "-c", "x"} },
		"a sidecar that runs ccsbx fwd": func(p *Pod) { sd(p).Command = []string{"ccsbx", "fwd"} },
		"a sidecar with 65 arguments": func(p *Pod) {
			sd(p).Command = append([]string{"/usr/local/bin/ccsbx", "side", "serve"}, make([]string, 62)...)
		},
		"an argument holding a NUL": func(p *Pod) { sd(p).Command = []string{"/usr/local/bin/ccsbx", "side", "serve", "a\x00b"} },
	} {
		q, _ := PodFor(s)
		mod(q)
		if err := DefaultConfig().CheckContract(q); err == nil {
			t.Errorf("%s passed the contract", why)
		}
	}
}

// The relay token reaches the sidecar in the load, in memory: there is no volume for it any more.
func TestThereIsNoRelayCredentialVolume(t *testing.T) {
	p := goodPod(t)
	for _, v := range p.Spec.Volumes {
		if v.Name == "relay-cred" {
			t.Error("the relay-cred volume is back")
		}
	}
	if _, ok := volumeOwner["relay-cred"]; ok {
		t.Error("relay-cred is in the volume-owner list")
	}
}

// a pod made with no sidecar command is refused at PodFor, not left to the image's entrypoint
func TestPodForRefusesASidecarWithNoCommand(t *testing.T) {
	s := goodSpec()
	s.SideCmd = nil
	if p, err := PodFor(s); err == nil {
		t.Errorf("a pod with no sidecar command was built: %v", p.Spec.InitContainers[0].Command)
	}
}

// the --scope value is built from a typed struct: the sidecar sees exactly the known keys, once, spelled
// exactly, whatever the record held
func TestTheScopeIsBuiltFromTheTypedStructOrRefused(t *testing.T) {
	scopeOf := func(raw string) (string, error) {
		i := goodSide()
		i.Scope = json.RawMessage(raw)
		a, err := SideArgv(i)
		if err != nil {
			return "", err
		}
		for k, x := range a {
			if x == "--scope" {
				return a[k+1], nil
			}
		}
		return "", nil
	}
	for why, raw := range map[string]string{
		"null":                        `null`,
		"an array":                    `[]`,
		"a duplicate manager":         `{"manager":{"node":"a","machine":"hp"},"manager":{"node":"b","machine":"mac"}}`,
		"a wrong-case key":            `{"Manager":{"node":"a","machine":"hp"}}`,
		"an unknown key":              `{"manager":{"node":"a","machine":"hp"},"extra":1}`,
		"a duplicate key in manager":  `{"manager":{"node":"a","node":"b","machine":"hp"}}`,
		"a wrong-case key in manager": `{"manager":{"Node":"a","machine":"hp"}}`,
		"a duplicate peer machine":    `{"manager":{"node":"a","machine":"hp"},"peers":["p"],"peer_machines":{"p":"mac","p":"hp"}}`,
		"peer_machines as a list":     `{"manager":{"node":"a","machine":"hp"},"peer_machines":[]}`,
		"a manager that is a string":  `{"manager":"a/b"}`,
		"trailing garbage":            `{"manager":{"node":"a","machine":"hp"}} {}`,
	} {
		if got, err := scopeOf(raw); err == nil {
			t.Errorf("%s was accepted: %s", why, got)
		}
	}
	got, err := scopeOf(`{ "reply_window_s": 3600, "peer_machines": {"p": "mac"}, "peers": ["p"], "manager": { "machine": "hp", "node": "a" } }`)
	if err != nil || got != `{"manager":{"node":"a","machine":"hp"},"peers":["p"],"peer_machines":{"p":"mac"},"reply_window_s":3600}` {
		t.Errorf("canonical form: %q %v", got, err)
	}
	// a record with no peers still gives peers:[] (never null)
	if got, _ := scopeOf(`{"manager":{"node":"a","machine":"hp"}}`); got != `{"manager":{"node":"a","machine":"hp"},"peers":[]}` {
		t.Errorf("%q", got)
	}
}

func TestADoorNameIsUsedOnce(t *testing.T) {
	i := goodSide()
	i.Doors = []string{"model=http://a:1", "model=http://b:2"}
	if a, err := SideArgv(i); err == nil {
		t.Errorf("two doors named model: %v", a)
	}
	i.Doors = []string{"model=http://a:1", "egress=http://b:2"}
	if _, err := SideArgv(i); err != nil {
		t.Error(err)
	}
}

// F3c: the sidecar clones into the EMPTY /work after the load, and refuses a non-empty one, so the fence must
// not start (and write there) until the sidecar says the worktree is ready; a failure shows its reason.
func TestTheFenceWaitsForTheWorktree(t *testing.T) {
	cmd := FenceEntry("s0abcdefgh")
	if len(cmd) != 5 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[4] != "s0abcdefgh" || strings.Contains(cmd[2], "s0abcdefgh") {
		t.Fatalf("%q (the sid is a positional argument, never part of the script)", cmd)
	}
	p, err := PodFor(goodSpec())
	if err != nil || strings.Join(p.Spec.Containers[0].Command, "\x00") != strings.Join(cmd, "\x00") {
		t.Errorf("the pod's fence command is not the waiting one: %v %v", p.Spec.Containers[0].Command, err)
	}
	// run the script for real with the in/ directory and tmux faked
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho \"tmux $*\"\n"), 0o755)
	script := strings.ReplaceAll(cmd[2], "/run/ccsbx/in", dir)
	run := func() (string, int) {
		c := exec.Command("/bin/sh", "-c", script, cmd[3], cmd[4])
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := c.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	os.WriteFile(filepath.Join(dir, "worktree-ready"), nil, 0o644)
	if out, code := run(); code != 0 || !strings.Contains(out, "tmux -f /dev/null new-session -s s0abcdefgh claude") {
		t.Errorf("ready: %d %q", code, out)
	}
	os.Remove(filepath.Join(dir, "worktree-ready"))
	os.WriteFile(filepath.Join(dir, "worktree-failed"), []byte("clone refused: nothing to clone\n"), 0o644)
	if out, code := run(); code != 1 || !strings.Contains(out, "could not be made") || !strings.Contains(out, "clone refused: nothing to clone") || strings.Contains(out, "tmux") {
		t.Errorf("failed: %d %q", code, out)
	}
}

// F3c R2-S1 and the pod's disk: a commit may fill the snapshot only to 90% of its volume, and the containers'
// ephemeral-storage limits must cover every disk-backed volume together, or the node evicts the pod first.
func TestTheSnapshotLimitAndTheEphemeralBudget(t *testing.T) {
	if got := SnapshotLimitBytes(10240); got != 9663676416 || got >= int64(10240)<<20 {
		t.Errorf("SnapshotLimitBytes(10240) = %d", got)
	}
	if DiskVolumesMB(10240) != 31336 {
		t.Errorf("DiskVolumesMB(10240) = %d", DiskVolumesMB(10240))
	}
	// the default budget covers the default volumes with room to spare, and the pod built from it says so
	s := goodSpec()
	if int64(s.EphemeralMB+SideEphemeralMB) < int64(DiskVolumesMB(s.WorktreeMB)) {
		t.Fatal("the test spec itself does not fit")
	}
	if _, err := PodFor(s); err != nil {
		t.Fatal(err)
	}
	// one MiB less than the total is refused, one more is not
	s.EphemeralMB = DiskVolumesMB(s.WorktreeMB) - SideEphemeralMB - 1
	if _, err := PodFor(s); err == nil || !strings.Contains(err.Error(), "ephemeral storage") {
		t.Errorf("a budget one MiB short was accepted: %v", err)
	}
	s.EphemeralMB++
	if _, err := PodFor(s); err != nil {
		t.Errorf("the exact budget was refused: %v", err)
	}
	// and a pod fetched back from the cluster is held to it too
	p := goodPod(t)
	p.Spec.Containers[0].Resources.Limits["ephemeral-storage"] = "20Gi"
	p.Spec.Containers[0].Resources.Requests["ephemeral-storage"] = "20Gi"
	if err := DefaultConfig().CheckContract(p); err == nil {
		t.Error("a pod whose volumes outgrow its ephemeral limit passed the contract")
	}
}
