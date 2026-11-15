package main

import (
	"os"
	"syscall"
)

const errorSharingViolation = syscall.Errno(32) // not exported by package syscall

// lockFile opens path with no sharing, so no one else can open it until the process exits.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err == errorSharingViolation {
		return nil, errLocked
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
