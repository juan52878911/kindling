package main

// kling db branch hook install|uninstall: el hook post-checkout de git que
// llama a `kling db branch -switch`.
//
// Idempotente y sin pisar nada: si ya hay un post-checkout que no es nuestro,
// se aparta a post-checkout.pre-kling-db y el nuestro lo ejecuta primero (y
// devuelve su código de salida). uninstall lo restaura. El hook nunca bloquea
// un checkout por culpa de kling: si -switch falla, avisa y sigue.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	hookMarker   = "# kling-db-branch-hook v1"
	hookName     = "post-checkout"
	hookOrigName = "post-checkout.pre-kling-db"
)

// hookScript va en sh POSIX. $3 = 1 solo en el checkout de una rama (los de
// ficheros no cambian de base). ${KLING:-kling} es el binario; -H y el dueño
// van por KLING_HOST y los valores por defecto.
const hookScript = `#!/bin/sh
` + hookMarker + ` (managed by "kling db branch hook"; uninstall with: kling db branch hook uninstall)
rc=0
d=$(dirname "$0")
if [ -x "$d/` + hookOrigName + `" ]; then
  "$d/` + hookOrigName + `" "$@" || rc=$?
fi
if [ "$3" = "1" ]; then
  k="${KLING:-kling}"
  if command -v "$k" >/dev/null 2>&1; then
    "$k" db branch -switch >&2 || echo "kling db: could not switch the database of this branch (the checkout is not affected)" >&2
  else
    echo "kling db: kling not found; the database was not switched" >&2
  fi
fi
exit $rc
`

func cmdBranchHook(action string) error {
	if action != "install" && action != "uninstall" {
		return usageErr("usage: kling db branch hook install|uninstall")
	}
	a := &app{stdout: os.Stdout, stderr: os.Stderr}
	ctx, stop := signalCtx()
	defer stop()
	return a.branchHook(ctx, action)
}

// hookDir es el directorio de hooks del repo (respeta core.hooksPath y los
// worktrees), absoluto.
func (a *app) hookDir(ctx context.Context) (string, error) {
	if _, err := a.repo(ctx); err != nil {
		return "", err
	}
	p, err := a.git(ctx, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		base := a.cwd
		if base == "" {
			if base, err = os.Getwd(); err != nil {
				return "", err
			}
		}
		p = filepath.Join(base, p)
	}
	return p, nil
}

func (a *app) branchHook(ctx context.Context, action string) error {
	dir, err := a.hookDir(ctx)
	if err != nil {
		return err
	}
	path, orig := filepath.Join(dir, hookName), filepath.Join(dir, hookOrigName)
	cur, err := readHook(path)
	if err != nil {
		return err
	}
	ours := cur != nil && strings.Contains(string(cur), hookMarker)

	if action == "uninstall" {
		if cur == nil || !ours {
			fmt.Fprintln(a.stderr, "the kling db hook is not installed; nothing to do")
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if _, err := os.Lstat(orig); err == nil {
			if err := os.Rename(orig, path); err != nil {
				return fmt.Errorf("restoring your previous hook: %w", err)
			}
			fmt.Fprintf(a.stdout, "removed the kling db hook; restored your previous %s\n", hookName)
			return nil
		}
		fmt.Fprintln(a.stdout, "removed the kling db hook")
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	chained := false
	if cur != nil && !ours {
		if _, err := os.Lstat(orig); err == nil {
			return fmt.Errorf("%s already exists next to a foreign %s: merge them by hand", orig, hookName)
		}
		if err := os.Rename(path, orig); err != nil {
			return err
		}
		chained = true
	}
	if err := writeExecutable(path, hookScript); err != nil {
		return err
	}
	if chained {
		fmt.Fprintf(a.stdout, "installed %s; your previous hook was kept as %s and runs first\n", path, hookOrigName)
	} else {
		fmt.Fprintf(a.stdout, "installed %s\n", path)
	}
	fmt.Fprintf(a.stdout, "every branch checkout now runs kling db branch -switch and writes the connection to\n"+
		"  <repo>/.git/%s  (0600, never in the working tree)\n"+
		"load it in your shell with:  set -a; . \"$(git rev-parse --absolute-git-dir)/%s\"; set +a\n", branchEnvFile, branchEnvFile)
	return nil
}

// readHook lee un hook existente. nil si no hay; un enlace o un no-fichero es
// un error (no se toca lo que no se entiende).
func readHook(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (symlink?): not touching it", path)
	}
	return os.ReadFile(path)
}

// writeExecutable escribe el fichero aparte y lo renombra, 0755.
func writeExecutable(path, body string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kling-db-hook.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
