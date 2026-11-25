//go:build unix

package aeh

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive lock on f without waiting. The lock belongs to the open
// file and ends when it is closed or the process dies.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// protectPrivateDir makes dir accessible to its owner only and checks that the owner is
// the current user, so a store created by someone else is never trusted.
func protectPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user (uid %d)", dir, st.Uid)
	}
	return os.Chmod(dir, 0o700)
}
