// Package revoke posts a signed revoke{owner,machine,sid,gen} to the doors' owner listeners. It exists so a
// generation that has ended (or never came up) kills its token at every door at once: the signer says who is
// revoking (the machine key signs, see signer.Client.SignRevoke), this posts what it signed. It holds no key and
// reads no secret; the body is posted exactly as signed.
package revoke

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Doors are the base URLs of the doors' owner listeners (token.RevokePath is appended).
type Doors struct {
	URLs    []string
	HTTP    *http.Client // nil: no redirects, 10 s
	Timeout time.Duration
}

// CheckURL: http or https, a host, no credentials, path, query or fragment.
func CheckURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, " \t\n\x00") {
		return fmt.Errorf("revoke door %q must be http(s)://host[:port] with no credentials, path or query", s)
	}
	return nil
}

func (d Doors) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	to := d.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	return &http.Client{Timeout: to, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Post sends the signed revoke to every door, whatever happens at the others, and returns what failed (the
// status code or the transport error, never a body). A door that answers 204 has it.
func (d Doors) Post(ctx context.Context, header string, body []byte) error {
	if len(d.URLs) == 0 {
		return errors.New("no revoke door is configured")
	}
	var errs []string
	for _, base := range d.URLs {
		if err := CheckURL(base); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+token.RevokePath, bytes.NewReader(body))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		req.Header.Set(token.RevokeHeader, header)
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.client().Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", base, errors.Unwrap(err)))
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			errs = append(errs, fmt.Sprintf("%s answered %d", base, resp.StatusCode))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
