package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// fileRoot uses the kernel's beneath-root resolution, including for symlinks.
// Linux 5.6+ is required (Debian Bookworm ships Linux 6.1). Fail closed when
// openat2 is unavailable rather than falling back to a racy path check.
type fileRoot struct{ dir *os.File }

func openRoot(name string) (*fileRoot, error) {
	fd, err := unix.Open(name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openroot", Path: name, Err: err}
	}
	return &fileRoot{os.NewFile(uintptr(fd), name)}, nil
}
func (r *fileRoot) Close() error { return r.dir.Close() }
func (r *fileRoot) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	mode := uint64(0)
	if flag&os.O_CREATE != 0 {
		mode = uint64(perm.Perm())
	}
	fd, err := unix.Openat2(int(r.dir.Fd()), name, &unix.OpenHow{
		Flags: uint64(flag | unix.O_CLOEXEC), Mode: mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (r *fileRoot) Open(name string) (*os.File, error) { return r.OpenFile(name, os.O_RDONLY, 0) }
func (r *fileRoot) Stat(name string) (os.FileInfo, error) {
	f, err := r.OpenFile(name, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
func (r *fileRoot) Mkdir(name string, perm os.FileMode) error {
	parent, err := r.OpenFile(filepath.Dir(name), unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Mkdirat(int(parent.Fd()), filepath.Base(name), uint32(perm.Perm()))
	if err != nil {
		return &os.PathError{Op: "mkdirat", Path: name, Err: err}
	}
	return nil
}
func (r *fileRoot) Remove(name string) error {
	parent, err := r.OpenFile(filepath.Dir(name), unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Unlinkat(int(parent.Fd()), filepath.Base(name), 0)
	if err != nil {
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}
