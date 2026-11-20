// reader.go holds the one helper that wraps bytes as a reader for the profile loader. It exists so
// the loader can parse from memory in tests without a temp file.

package profile

import (
	"bytes"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
