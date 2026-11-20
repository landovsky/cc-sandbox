// export.go is the part of the outbox checks the sidecar relay (F3) shares with the daemon. It exists so
// there is ONE strict-JSON type check, ONE refusal vocabulary and ONE request-name rule: a second copy
// in the sidecar could drift, and the fence picks which one reads its request.
package hostd

// StrictType returns a request's "type", refusing duplicate top-level keys and trailing data.
func StrictType(data []byte) (string, error) { return strictType(data) }

// Classify maps a read error to the stable refusal code (never file content).
func Classify(err error) string { return classify(err) }

// RequestRid returns the rid of a request file name, and false for anything that is not one
// (.tmp files, odd names).
func RequestRid(name string) (string, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}
