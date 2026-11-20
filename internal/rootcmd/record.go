// Loading a session record for ccsbx-root. The record is written by the daemon (hostd, uid ccsbx),
// which is less trusted than root, so the file is read without following links, must be owned by
// root or the service uid, and every field root acts on is validated again here: root believes the
// record only as far as it can check it.
package rootcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/start"
)

const maxRecord = 1 << 20

// errNoRecord: no record file for the sid.
var errNoRecord = fmt.Errorf("no record")

// loadRecord reads <state>/sessions/<sid>.json. serviceUID is the only uid besides root that may
// own it.
func loadRecord(stateDir, sid string, serviceUID, trustedUID int) (*start.Record, error) {
	if !start.ValidSid(sid) {
		return nil, fmt.Errorf("%q is not a session id", sid)
	}
	dir := filepath.Join(stateDir, "sessions")
	if err := ownedBy(dir, serviceUID, trustedUID); err != nil {
		return nil, fmt.Errorf("session records: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, sid+".json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errNoRecord
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxRecord {
		return nil, fmt.Errorf("record of %s is not a regular file under %d bytes", sid, maxRecord)
	}
	if err := ownerOK(st, serviceUID, trustedUID); err != nil {
		return nil, fmt.Errorf("record of %s: %w", sid, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if err != nil {
		return nil, err
	}
	var rec start.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("record of %s: %v", sid, err)
	}
	if rec.Sid != sid {
		return nil, fmt.Errorf("record file %s.json names session %q", sid, rec.Sid)
	}
	if rec.Gen < 1 || rec.Gen > 9999 {
		return nil, fmt.Errorf("record of %s: gen %d out of range", sid, rec.Gen)
	}
	if rec.Uid < 0 {
		return nil, fmt.Errorf("record of %s: uid %d", sid, rec.Uid)
	}
	return &rec, nil
}

// ownedBy checks a path (not followed through a link) is owned by root-or-service and not
// writable by group or others.
func ownedBy(path string, serviceUID, trustedUID int) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	return ownerOK(st, serviceUID, trustedUID)
}

func ownerOK(st os.FileInfo, serviceUID, trustedUID int) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no owner information, refusing")
	}
	if u := int(sys.Uid); u != trustedUID && u != serviceUID {
		return fmt.Errorf("owned by uid %d, which is neither root nor the service user", u)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("writable by group or others (%v)", st.Mode().Perm())
	}
	return nil
}
