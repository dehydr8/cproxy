//go:build !unix

package daemon

// Lock is best-effort where flock is unavailable: a second daemon fails to
// take the control socket instead.
func Lock() (release func(), err error) { return func() {}, nil }
