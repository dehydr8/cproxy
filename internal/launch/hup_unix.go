//go:build unix

package launch

import (
	"os"
	"os/signal"
	"syscall"
)

// NotifyHUP subscribes ch to SIGHUP for serve-mode config reloads.
func NotifyHUP(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
