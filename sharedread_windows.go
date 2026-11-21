//go:build windows

// sharedread_windows.go — opening a file that another process holds open.
//
// WHY THIS EXISTS: a Chromium browser keeps its cookie store open while it runs,
// and on Windows such a handle blocks an ordinary read with
// "The process cannot access the file … because it is being used by another
// process". The operator's everyday browser is open most of the time, so the
// plain read fails exactly when it matters most.
//
// The fix is the one Windows offers for this case: open with FILE_SHARE_READ
// (and FILE_SHARE_DELETE), which succeeds while the other process holds its
// handle for writing. The handle is READ-ONLY: nothing in this plugin ever
// writes to a browser profile.
//
// Measured 2026-11-21 on this box: the default Edge profile's cookie store is
// locked by the running browser, and this open is what makes its 秒悟 session
// readable.
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32DLL          = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW      = kernel32DLL.NewProc("CreateFileW")
	procSetFilePointerEx = kernel32DLL.NewProc("SetFilePointerEx")
	procGetFileSizeEx    = kernel32DLL.NewProc("GetFileSizeEx")
)

// FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE
const sharedReadShareMode = 0x00000001 | 0x00000002 | 0x00000004

// GENERIC_READ
const genericRead = 0x80000000

// OPEN_EXISTING
const openExisting = 3

// FILE_ATTRIBUTE_NORMAL
const fileAttributeNormal = 0x00000080

// invalidHandleValue is the sentinel CreateFileW returns on failure.
const invalidHandleValue = ^uintptr(0)

// largeFileSize is the sentinel GetFileSizeEx returns on failure.
const largeFileSize = ^uint64(0)

// openSharedRead opens a file for reading while other processes may hold it.
//
// It returns an *os.File so the caller can use it like any other file. The
// handle is opened with a shared read/write/delete mode, which is what lets a
// locked browser store be copied.
func openSharedRead(path string) (*os.File, error) {
	pathPtr, errUTF := syscall.UTF16PtrFromString(path)
	if errUTF != nil {
		return nil, errUTF
	}
	handle, _, lastErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(genericRead),
		uintptr(sharedReadShareMode),
		0, // security attributes
		uintptr(openExisting),
		uintptr(fileAttributeNormal),
		0, // template file
	)
	if handle == invalidHandleValue {
		return nil, fmt.Errorf("open %s for shared reading: %w", path, lastErr)
	}
	file := os.NewFile(handle, path)
	if file == nil {
		_ = syscall.CloseHandle(syscall.Handle(handle))
		return nil, fmt.Errorf("open %s: could not wrap the handle", path)
	}
	return file, nil
}
