// Package profiles embeds the approved session profiles (the JSON files next to this file). It
// exists so that ccsbx-root, which runs as root, reads the memory and pid caps and the deny list
// from the root-owned, checksummed binary instead of from a repo checkout the owner's agents can
// edit. Changing a profile therefore means a rebuild and a reinstall.
package profiles

import (
	"embed"
	"fmt"

	"github.com/landovsky/cc-sandbox/internal/profile"
)

//go:embed *.json
var files embed.FS

// Load returns the embedded profile with this name, validated.
func Load(name string) (*profile.Profile, error) {
	// the name is checked before it touches the embedded tree, as profile.Load does for a dir
	if !profile.ValidName(name) {
		return nil, fmt.Errorf("profile name %q is not valid", name)
	}
	b, err := files.ReadFile(name + ".json")
	if err != nil {
		return nil, fmt.Errorf("profile %q is not approved in this build", name)
	}
	p, err := profile.Parse(b)
	if err != nil {
		return nil, err
	}
	if p.Name != name {
		return nil, fmt.Errorf("profile file %s.json names %q", name, p.Name)
	}
	return p, nil
}
