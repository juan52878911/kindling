//go:build !unix

package main

import (
	"os"
	"syscall"
)

func detached() *syscall.SysProcAttr { return nil }

func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
}
