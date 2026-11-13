//go:build unix

package aeh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// runProcessTree gives the command its own group, including descendants
// that inherit it, so cancellation cannot signal the harness's process group.
func runProcessTree(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return fmt.Errorf("kill process group %d: %w", cmd.Process.Pid, err)
		}
		return nil
	}
	return cmd.Run()
}
