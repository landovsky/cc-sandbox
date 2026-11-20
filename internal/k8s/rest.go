// rest.go is the REST client for the launcher's few verbs (K8: pods create/get/list/delete in one
// namespace), over net/http with a bearer token and the cluster CA. It exists because client-go is a
// third-party module; the API surface we need is four calls. It reads a small JSON file, not a
// kubeconfig (that is YAML): {"server": "https://...", "ca_file": "...", "token_file": "..."}, which
// the installer derives from the launcher kubeconfig that lives under ccsbx-keys. The token is read
// on every request, so a renewal (the launcher token is short-lived) needs no restart.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound: the API answered 404.
var ErrNotFound = errors.New("not found")

// APIError is any other non-2xx answer.
type APIError struct {
	Status int
	Reason string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes API %d %s: %s", e.Status, e.Reason, e.Msg)
}

// ConnConfig is the JSON file.
type ConnConfig struct {
	Server    string `json:"server"`
	CAFile    string `json:"ca_file"`
	TokenFile string `json:"token_file"`
}

// LoadConnConfig reads and validates the file.
func LoadConnConfig(path string) (*ConnConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c ConnConfig
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%s: server %q must be an https URL", path, c.Server)
	}
	if c.TokenFile == "" {
		return nil, fmt.Errorf("%s: token_file is required", path)
	}
	return &c, nil
}

// REST is the Client over HTTP.
type REST struct {
	Base      string
	TokenFile string
	HTTP      *http.Client
}

// NewREST builds a client; with CAFile set the cluster CA is the only root trusted.
func NewREST(c *ConnConfig) (*REST, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate in it", c.CAFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &REST{Base: strings.TrimRight(c.Server, "/"), TokenFile: c.TokenFile, HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func (r *REST) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.Base+path, rd)
	if err != nil {
		return err
	}
	tok, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return fmt.Errorf("launcher token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		var st struct{ Reason, Message string }
		json.Unmarshal(b, &st)
		return &APIError{Status: resp.StatusCode, Reason: st.Reason, Msg: st.Message}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func podsPath(ns string) string { return "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods" }

func (r *REST) CreatePod(ctx context.Context, ns string, p *Pod) (*Pod, error) {
	var out Pod
	if err := r.do(ctx, "POST", podsPath(ns), p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *REST) GetPod(ctx context.Context, ns, name string) (*Pod, error) {
	var out Pod
	if err := r.do(ctx, "GET", podsPath(ns)+"/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *REST) ListPods(ctx context.Context, ns, sel string) ([]Pod, error) {
	var out struct{ Items []Pod }
	if err := r.do(ctx, "GET", podsPath(ns)+"?labelSelector="+url.QueryEscape(sel), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (r *REST) DeletePod(ctx context.Context, ns, name string, grace int) error {
	err := r.do(ctx, "DELETE", podsPath(ns)+"/"+url.PathEscape(name)+"?gracePeriodSeconds="+strconv.Itoa(grace), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
