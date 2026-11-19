package start

// NoLauncher records the session and starts nothing. Tests and debugging use it, then run
// `ccsbx run --sid <sid>` themselves. It reports every session as not alive, so a record made
// this way never blocks its label.
type NoLauncher struct{}

func (NoLauncher) Launch(*Record) error { return nil }
func (NoLauncher) Alive(string) bool    { return false }
