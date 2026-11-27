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
	getNamedSecurityInfo = sessionAdvapi32.NewProc("GetNamedSecurityInfoW")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)

	sddlRevision1             = 1
	seFileObject              = 1
	ownerSecurityInformation  = 0x1
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
// protected individually before use, so no entry keeps access of its own. The current
// user also becomes the owner, and the owner is read back: an owner can always change
// the access list again, so an entry whose ownership cannot be taken is refused.
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
	if r, _, _ := setNamedSecurityInfo.Call(uintptr(unsafe.Pointer(name)), seFileObject,
		ownerSecurityInformation|daclSecurityInformation|protectedDaclSecurityInfo, uintptr(unsafe.Pointer(user.User.Sid)), 0, dacl, 0); r != 0 {
		return fmt.Errorf("protect %s: %w", p, syscall.Errno(r))
	}
	owner, err := fileOwner(p)
	if err != nil {
		return fmt.Errorf("protect %s: %w", p, err)
	}
	if owner != sid {
		return fmt.Errorf("protect %s: owned by %s, not by the current user", p, owner)
	}
	return nil
}

// fileOwner is the SID of the owner of p, as a string.
func fileOwner(p string) (string, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	var owner *syscall.SID
	var sd uintptr
	if r, _, _ := getNamedSecurityInfo.Call(uintptr(unsafe.Pointer(name)), seFileObject, ownerSecurityInformation,
		uintptr(unsafe.Pointer(&owner)), 0, 0, 0, uintptr(unsafe.Pointer(&sd))); r != 0 {
		return "", syscall.Errno(r)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	return owner.String()
}
