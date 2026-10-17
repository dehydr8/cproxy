//go:build unix

package launch

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Run starts the wrapped command with the given environment, keeps cproxy
// alive until it exits, and returns its exit code.
//
// The child stays in cproxy's process group, so terminal signals (Ctrl+C)
// reach it directly from the terminal; cproxy ignores them and keeps serving
// until the child decides to exit. A SIGTERM aimed at cproxy itself is
// forwarded to the child.
func Run(argv []string, env []string) (int, error) {
	if len(argv) == 0 {
		return 1, errors.New("no command to run")
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return 1, err
	}

	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	for {
		select {
		case err := <-done:
			return exitCode(err), nil
		case <-term:
			if c.Process != nil {
				c.Process.Signal(syscall.SIGTERM)
			}
		}
	}
}

// exitCode maps a Wait result to the child's exit code (128+n for signals).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return 128 + int(st.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}
