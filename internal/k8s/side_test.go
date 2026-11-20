// Tests for the sidecar's command line: exact flags from the record, nothing the fence can write, every
// value checked because the pod spec is read by the cluster, and the contract holds the command to
// `ccsbx side serve`.
package k8s

import (
	"encoding/json"
	"strings"
	"testing"
)

func goodSide() SideInfo {
	return SideInfo{Sid: "s0abcdefgh", Gen: 2, Label: "cc-sandbox:x", FullID: "tomas/future-two/s0abcdefgh/g2", Machine: "future-two",
		Scope: json.RawMessage(`{ "manager": {"node": "cc-sandbox:#6b3s", "machine": "hp-ubuntu"}, "peers": [] }`), HubURL: "http://hub-egress.cc-sandbox.svc:4011",
		Doors: []string{"model=http://ccsp-model.cc-sandbox-doors.svc:8080"}}
}

func TestSideArgvIsExactlyWhatTheSidecarsPathsAndRecordSay(t *testing.T) {
	got, err := SideArgv(goodSide())
	if err != nil {
		t.Fatal(err)
	}
	want := "/usr/local/bin/ccsbx side serve --listen 127.0.0.1:8080 --control /run/ccsbx/side/ctl/ctl.sock --state /run/ccsbx/side/state --journal-dir /run/ccsbx/journal/s " +
		"--out /run/ccsbx/out --in /run/ccsbx/in --fence-uid 10001 --sid s0abcdefgh --gen 2 --label cc-sandbox:x --full-id tomas/future-two/s0abcdefgh/g2 " +
		`--machine future-two --scope {"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":[]} ` +
		"--hub-url http://hub-egress.cc-sandbox.svc:4011 --door model=http://ccsp-model.cc-sandbox-doors.svc:8080"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
	// no hub URL until the egress Service exists: the flag is left out, not empty
	i := goodSide()
	i.HubURL, i.Doors = "", nil
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
