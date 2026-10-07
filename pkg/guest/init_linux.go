//go:build linux

package guest

import (
	"io/fs"
	"os"
	"syscall"
)

// linuxInit son las llamadas de verdad del init (init.go).
type linuxInit struct{}

func osInitSys() (initSys, error) { return linuxInit{}, nil }

func (linuxInit) Mount(src, target, fstype string, flags uintptr, data string) error {
	return syscall.Mount(src, target, fstype, flags, data)
}

func (linuxInit) Unmount(target string) error               { return syscall.Unmount(target, 0) }
func (linuxInit) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(p, perm) }
func (linuxInit) PivotRoot(newRoot, putOld string) error    { return syscall.PivotRoot(newRoot, putOld) }
func (linuxInit) Chdir(dir string) error                    { return os.Chdir(dir) }
func (linuxInit) ReadFile(p string) ([]byte, error)         { return os.ReadFile(p) }
func (linuxInit) WriteFile(p string, b []byte) error        { return os.WriteFile(p, b, 0o644) }
func (linuxInit) Symlink(target, p string) error            { return os.Symlink(target, p) }

func (linuxInit) Stat(p string) error {
	_, err := os.Stat(p)
	return err
}

func (linuxInit) Lstat(p string) error {
	_, err := os.Lstat(p)
	return err
}

func (linuxInit) AppendFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (linuxInit) Exec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }
