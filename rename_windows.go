//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// renameReplace atomically replaces newname with oldname. Go's os.Rename maps
// to MoveFileEx(MOVEFILE_REPLACE_EXISTING), which returns ERROR_ACCESS_DENIED
// when newname has an open handle even if that handle shared delete:
// FILE_SHARE_DELETE is necessary but not sufficient. Only a POSIX-semantics
// rename replaces over an open file, so fall back to
// SetFileInformationByHandle(FileRenameInfoEx) for exactly that case. Every
// other error keeps MoveFileEx's result. If both attempts fail, retain both
// causes so callers can diagnose the fallback and match either error.
func renameReplace(oldname, newname string) error {
	err := os.Rename(oldname, newname)
	if !isOpenHandleReplaceErr(err) {
		return err
	}
	if fallbackErr := posixRename(oldname, newname); fallbackErr != nil {
		return errors.Join(err, fallbackErr)
	}
	return nil
}

// isOpenHandleReplaceErr reports whether err is the refusal a POSIX-semantics
// rename can overcome. ERROR_SHARING_VIOLATION is included because Windows
// reports the same open-destination condition under that code in some paths.
func isOpenHandleReplaceErr(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// posixRename renames by handle, so an open destination is replaced the way
// POSIX rename(2) does. The source handle must carry DELETE access and share
// delete (openForRename); the destination reader must hold FILE_SHARE_DELETE
// (openShared) — the POSIX-semantics flag is what actually permits the replace,
// not the share alone.
func posixRename(oldname, newname string) error {
	src, err := openForRename(oldname)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	buf, err := renameInfoBuffer(newname)
	if err != nil {
		return err
	}
	if len(buf) == 0 {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: errors.New("empty rename buffer")}
	}
	err = windows.SetFileInformationByHandle(windows.Handle(src.Fd()),
		windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: err}
	}
	return nil
}

// openForRename opens the rename source: DELETE is what SetFileInformation
// requires, and share delete lets concurrent readers coexist.
func openForRename(path string) (*os.File, error) {
	name, err := windowsPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// fileRenameInfo mirrors Win32 FILE_RENAME_INFO_EX for
// SetFileInformationByHandle(FileRenameInfoEx). The name bytes follow the
// struct; the FILE_RENAME flags reuse the union that ReplaceIfExists occupied.
// See https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-file_rename_info_ex .
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// renameInfoBuffer serializes FILE_RENAME_INFO for FileRenameInfoEx, with the
// destination as a long-path-aware UTF-16 name.
func renameInfoBuffer(newname string) ([]byte, error) {
	path, err := longPath(newname)
	if err != nil {
		return nil, err
	}
	name := windows.StringToUTF16(path)
	var info fileRenameInfo
	offset := int(unsafe.Offsetof(info.FileName))
	// FileNameLength excludes the trailing NUL the kernel still reads.
	nameBytes := (len(name) - 1) * 2
	buf := make([]byte, offset+len(name)*2)
	// Byte-wise stores: buf has no alignment guarantee, so uint32 stores
	// through unsafe.Pointer are undefined behavior here.
	binary.LittleEndian.PutUint32(buf[0:],
		windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
	binary.LittleEndian.PutUint32(buf[unsafe.Offsetof(info.FileNameLength):], uint32(nameBytes))
	copy(buf[offset:], unsafe.Slice((*byte)(unsafe.Pointer(&name[0])), len(name)*2))
	return buf, nil
}
