//go:build !unix

package launch

import (
	"errors"
	"os/exec"
)

// Run starts the wrapped command with the given environment and returns its
// exit code. Windows has no terminal signal semantics to mirror.
func Run(argv []string, env []string) (int, error) {
	if len(argv) == 0 {
		return 1, errors.New("no command to run")
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = env
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
