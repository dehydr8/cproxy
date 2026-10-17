//go:build !unix

package launch

import "os"

// NotifyHUP is a no-op where SIGHUP does not exist.
func NotifyHUP(chan<- os.Signal) {}
