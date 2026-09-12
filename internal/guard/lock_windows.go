package guard

import (
	"os"
	"syscall"
	"unsafe"
)

func lockStateFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceStateFile(from, to string) error {
	source, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// Replace the complete file and wait for the filesystem to flush the move.
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(source)), uintptr(unsafe.Pointer(target)), 0x1|0x8)
	if result == 0 {
		return callErr
	}
	return nil
}
