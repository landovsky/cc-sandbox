package hostd

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// Why a file under an outbox was not read. These are constants on purpose: a refusal names
// the rule, never the bytes of the request (the fence is untrusted).
var (
	ErrNotRegular = errors.New("not_regular")
	ErrHardlink   = errors.New("hardlink")
	ErrOwner      = errors.New("wrong_owner")
	ErrTooBig     = errors.New("too_big")
	ErrUnsafePath = errors.New("unsafe_path")
)

// Root is an opened directory the daemon reads beneath (an outbox).
type Root struct {
	fd   int
	path string
}

var (
	rootsMu sync.Mutex
	roots   = map[int]string{}
)

func rootPath(fd int) string { rootsMu.Lock(); defer rootsMu.Unlock(); return roots[fd] }

// OpenRoot opens dir itself, with a plain open: dir comes from the daemon's own state tree,
// not from the fence.
func OpenRoot(dir string) (*Root, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	rootsMu.Lock()
	roots[fd] = dir
	rootsMu.Unlock()
	return &Root{fd: fd, path: dir}, nil
}

func (r *Root) Close() error {
	rootsMu.Lock()
	delete(roots, r.fd)
	rootsMu.Unlock()
	return syscall.Close(r.fd)
}

// Limits are what a file must satisfy before a byte of it is read.
type Limits struct {
	Uid int   // the fence's uid on the host (10001 with the real backend)
	Max int64 // size cap in bytes
}

// ReadFile opens rel beneath the root without following any symlink, then judges the opened
// file (not the path): a regular file, one link, owned by the fence uid, within the size cap.
// Only then does it read, and never more than Max+1 bytes. Opening is O_NONBLOCK, so a FIFO is
// rejected by the fstat instead of hanging the daemon on open or read.
func (r *Root) ReadFile(rel string, lim Limits) ([]byte, uint64, error) {
	fd, err := openBeneath(r.fd, rel, syscall.O_RDONLY)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EXDEV) {
			return nil, 0, ErrUnsafePath
		}
		return nil, 0, err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, 0, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, 0, ErrNotRegular
	}
	if uint64(st.Nlink) != 1 {
		return nil, 0, ErrHardlink
	}
	if int(st.Uid) != lim.Uid {
		return nil, 0, ErrOwner
	}
	if st.Size > lim.Max {
		return nil, 0, ErrTooBig // a sparse file's apparent size counts: it is refused unread
	}
	buf := make([]byte, 0, st.Size)
	chunk := make([]byte, 4096)
	for int64(len(buf)) <= lim.Max {
		n, err := syscall.Read(fd, chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN {
			break // a regular file never blocks; stop rather than spin
		}
		if err != nil {
			return nil, 0, err
		}
		if n == 0 {
			break
		}
	}
	if int64(len(buf)) > lim.Max {
		return nil, 0, ErrTooBig // it grew after the fstat
	}
	return buf, uint64(st.Ino), nil
}

// ListDir returns up to max names of the directory rel (a directory under the root, opened
// the same way), plus whether there were more. Names only: nothing is statted or read.
func (r *Root) ListDir(rel string, max int) (names []string, more bool, err error) {
	fd, err := openBeneath(r.fd, rel, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), "dir")
	defer f.Close()
	for len(names) < max {
		ents, err := f.ReadDir(256)
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if err != nil {
			return names, false, nil // io.EOF
		}
		if len(ents) == 0 {
			return names, false, nil
		}
	}
	return names, true, nil
}

// ReadRange reads at most max bytes of rel starting at off, under the same rules as ReadFile
// (regular, one link, fence-owned, no symlink) but with NO size cap on the file itself: it is
// for append-only streams (events.jsonl) read in bounded chunks. It returns the bytes and the
// file's size at fstat time.
func (r *Root) ReadRange(rel string, off int64, max int, lim Limits) ([]byte, int64, error) {
	fd, err := openBeneath(r.fd, rel, syscall.O_RDONLY)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EXDEV) {
			return nil, 0, ErrUnsafePath
		}
		return nil, 0, err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, 0, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, 0, ErrNotRegular
	}
	if uint64(st.Nlink) != 1 {
		return nil, 0, ErrHardlink
	}
	if int(st.Uid) != lim.Uid {
		return nil, 0, ErrOwner
	}
	if off >= st.Size {
		return nil, st.Size, nil
	}
	n := int64(max)
	if st.Size-off < n {
		n = st.Size - off
	}
	buf := make([]byte, n)
	got := 0
	for got < len(buf) {
		m, err := syscall.Pread(fd, buf[got:], off+int64(got))
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if m == 0 {
			break
		}
		got += m
	}
	return buf[:got], st.Size, nil
}
