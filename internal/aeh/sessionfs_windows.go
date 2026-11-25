//go:build windows

package aeh

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	lockFileEx           = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	sessionAdvapi32      = syscall.NewLazyDLL("advapi32.dll")
	convertSDDL          = sessionAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	getSecurityDescDacl  = sessionAdvapi32.NewProc("GetSecurityDescriptorDacl")
	setNamedSecurityInfo = sessionAdvapi32.NewProc("SetNamedSecurityInfoW")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)

	sddlRevision1             = 1
	seFileObject              = 1
	daclSecurityInformation   = 0x4
	protectedDaclSecurityInfo = 0x80000000
)

// tryLockFile takes an exclusive lock on f without waiting. The lock belongs to the open
// handle and ends when it is closed or the process dies.
func tryLockFile(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	r, _, err := lockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return true, nil
	}
	if err == errorLockViolation {
		return false, nil
	}
	return false, err
}

// protectPrivate replaces the access list of a directory or file of the session store
// with a protected one that grants access to the current user and SYSTEM only (inherited
// by what a directory holds): Unix modes do not restrict access on Windows, a checkout's
// inherited permissions may include other users, and an entry that existed before may
// carry explicit grants or a protected list of its own. Each directory and file is
// protected individually before use, so no entry keeps access of its own.
func protectPrivate(p string, dir bool) error {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	defer tok.Close()
	user, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	sddl, err := syscall.UTF16PtrFromString("D:P(A;" + inherit + ";FA;;;" + sid + ")(A;" + inherit + ";FA;;;SY)")
	if err != nil {
		return err
	}
	var sd uintptr
	if r, _, err := convertSDDL.Call(uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	var present, defaulted int32
	var dacl uintptr
	if r, _, err := getSecurityDescDacl.Call(sd, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); r == 0 {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	if r, _, _ := setNamedSecurityInfo.Call(uintptr(unsafe.Pointer(name)), seFileObject, daclSecurityInformation|protectedDaclSecurityInfo, 0, 0, dacl, 0); r != 0 {
		return fmt.Errorf("protect %s: %w", p, syscall.Errno(r))
	}
	return nil
}
