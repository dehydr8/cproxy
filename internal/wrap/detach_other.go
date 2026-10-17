//go:build !unix

package wrap

import "os/exec"

// detach is a no-op where process groups do not exist.
func detach(*exec.Cmd) {}
