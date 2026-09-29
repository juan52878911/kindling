//go:build unix

package dbstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcluye(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLING_DB_STATE", root)
	ctx := context.Background()
	un, err := Lock(ctx, "branch-abc", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// flock va por descripción de fichero abierta: un segundo Lock del mismo
	// proceso choca igual que el de otro proceso.
	waited := false
	if _, err := Lock(ctx, "branch-abc", 120*time.Millisecond, func() { waited = true }); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if !waited {
		t.Error("the waiting callback was not called")
	}
	// Otro nombre no choca.
	un2, err := Lock(ctx, "branch-def", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	un2()
	// Al soltarlo, el que espera entra.
	done := make(chan error, 1)
	go func() {
		u, err := Lock(ctx, "branch-abc", 5*time.Second, nil)
		if err == nil {
			u()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	un()
	if err := <-done; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	st, err := os.Stat(filepath.Join(root, LocksDir, "branch-abc.lock"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(root, LocksDir)); st.Mode().Perm() != 0o700 {
		t.Errorf("locks dir mode %v", st.Mode().Perm())
	}
}

func TestLockCancelado(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	un, err := Lock(context.Background(), "x", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer un()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lock(ctx, "x", time.Minute, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestLockNombreYEnlace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLING_DB_STATE", root)
	for _, n := range []string{"", "../x", "A", "a/b", "-x"} {
		if _, err := Lock(context.Background(), n, 0, nil); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, LocksDir), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, LocksDir, "l.lock")); err != nil {
		t.Skip(err)
	}
	if _, err := Lock(context.Background(), "l", 0, nil); err == nil {
		t.Fatal("it followed a symlink")
	}
}
