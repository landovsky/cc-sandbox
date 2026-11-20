// remote.go is the two routes of the remote start (M2c, docs/m2c-remote-start.md): POST /v1/sessions, which
// another machine's owner calls to start a session HERE, and GET /v1/resolve?v=2, which another machine's
// daemon calls to learn whether a label is live or being started here. It exists apart from api.go because
// both are tailnet-only in practice and every refusal they make has the contract's one shape.
package daemonapi

import (
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
)

var labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_./-]{0,79}$`)

// createSession: POST /v1/sessions. Owner class only (the route table already refuses a peer; this is the second
// lock), on the tailnet only: the local unix socket starts sessions through /v1/start, as the caller's own uid.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if s.Remote == nil || !s.Remote.Enabled() {
		refuse(w, 404, "not_found", "this daemon does not serve remote starts")
		return
	}
	id := identOf(r)
	if classOf(r) != ClassOwner || id.Login == "" {
		refuse(w, 403, start.Forbidden, "only the owner may start a session remotely")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10+1))
	if err != nil || len(body) > 256<<10 {
		refuse(w, 400, start.BadSpec, "the spec is unreadable or larger than 256 KiB")
		return
	}
	caller := remotestart.Caller{StartedBy: "owner:" + id.Login + "@" + id.Short, Machine: id.Device}
	rep, ref := s.Remote.Start(r.Context(), caller, body)
	if ref != nil {
		if ref.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(ref.RetryAfter))
		}
		writeJSON(w, remotestart.Status(ref.Code), ref)
		return
	}
	writeJSON(w, 200, rep)
}

// resolveV2: GET /v1/resolve?label=<label>&v=2, for another daemon (peer class) or the owner: is the label live here,
// is a start of it in progress here, and which set of machines is this daemon configured with.
func (s *Server) resolveV2(w http.ResponseWriter, r *http.Request) {
	if s.Remote == nil {
		refuse(w, 404, "not_found", "this daemon does not serve the label check")
		return
	}
	if !isLocal(r) && classOf(r) < ClassPeer {
		refuse(w, 403, start.Forbidden, "peer or owner only")
		return
	}
	label := r.URL.Query().Get("label")
	if !labelRe.MatchString(label) {
		refuse(w, 400, start.BadSpec, "label is not valid")
		return
	}
	a, err := s.Remote.Answer(label)
	if err != nil { // could not tell: never "free"
		refuse(w, 500, remotestart.Internal, "this machine cannot tell whether the label is live")
		return
	}
	writeJSON(w, 200, a)
}
