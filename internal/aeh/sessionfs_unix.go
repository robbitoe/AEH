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

// protectPrivate makes a directory (0700) or file (0600) of the session store accessible
// to its owner only and checks that the owner is the current user, so a store created by
// someone else is never trusted.
func protectPrivate(p string, dir bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user (uid %d)", p, st.Uid)
	}
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	return os.Chmod(p, mode)
}
