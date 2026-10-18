//go:build unix

package daemon

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"github.com/dehydr8/cproxy/internal/config"
)

// Lock takes the per-user daemon lock (flock on LockPath), or returns
// ErrRunning when another daemon holds it. The lock dies with the process.
func Lock() (release func(), err error) {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
}
