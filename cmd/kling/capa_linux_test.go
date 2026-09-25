//go:build linux

package main

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCapaPequenaSeEncogeSana construye con 81-base-image.sh una capa como la
// de un Chispa —pequeña y casi llena: unos 20 MiB de binarios en GROW=33— y
// comprueba que sale encogida y sana, y que después se puede agrandar (lo que
// hace crecerImagen en un refresco).
//
// Antes, `resize2fs -M` dejaba estas capas con «Resize inode not valid» en
// e2fsprogs 1.47.0 (Ubuntu 24.04, la VM de Lima), y el constructor chispa
// fallaba. Necesita root, loop y overlay: fuera del laboratorio se salta. En
// el Mac se compila con GOOS=linux y se corre en la VM:
//
//	GOOS=linux GOARCH=arm64 go test -c -o kling.test ./cmd/kling
//	sudo KLING_BASE_IMAGE_SCRIPT=/tmp/81-base-image.sh ./kling.test -test.run CapaPequena -test.v
func TestCapaPequenaSeEncogeSana(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (loop and overlay mounts)")
	}
	for _, c := range []string{"mkfs.ext4", "e2fsck", "resize2fs", "dumpe2fs", "mount"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("no %s on this host", c)
		}
	}
	script := envOr("KLING_BASE_IMAGE_SCRIPT", filepath.Join("..", "..", "scripts", "81-base-image.sh"))
	if _, err := os.Stat(script); err != nil {
		t.Skipf("no build script: %v", err)
	}
	agent, err := exec.LookPath("true") // un ELF pequeño hace de kling-guest
	if err != nil {
		t.Skip("no /bin/true")
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "kroot")
	mustMkdir(t, filepath.Join(root, "images"))
	// La base: un ext4 con el esqueleto de una raíz; lo que lleve da igual.
	baseTree := filepath.Join(dir, "base")
	for _, d := range []string{"bin", "etc", "usr/local/bin", "opt", "proc"} {
		mustMkdir(t, filepath.Join(baseTree, d))
	}
	run(t, "mkfs.ext4", "-q", "-F", "-d", baseTree, filepath.Join(root, "images", "min.ext4"), "16M")

	// Lo que mete el constructor chispa: kling-chispa (~10 MB), el modelo y un
	// servicio. Aleatorio para que no se comprima ni se deduplique nada.
	rootfs := filepath.Join(dir, "rootfs")
	mustMkdir(t, filepath.Join(rootfs, "opt", "chispa"))
	mustMkdir(t, filepath.Join(rootfs, "models"))
	mustMkdir(t, filepath.Join(rootfs, "etc", "chispa"))
	writeRandom(t, filepath.Join(rootfs, "opt", "chispa", "kling-chispa"), 19<<20)
	writeRandom(t, filepath.Join(rootfs, "models", "task.chispa"), 600<<10)
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "chispa", "run.sh"), []byte("#!/bin/sh\nexec sleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	const grow = 33
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "KLING_ROOT="+root, "NAME=chispa-test", "BASE=min",
		"GROW="+strconv.Itoa(grow), "PKGS=", "AGENT="+agent, "ENV_FILE="+envFile,
		"ROOTFS_DIR="+rootfs, "SERVICE=/etc/chispa/run.sh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the layer: %v\n%s", err, out)
	} else if strings.Contains(string(out), "AVISO") {
		t.Errorf("the layer was not shrunk:\n%s", out)
	}

	layer := filepath.Join(root, "images", "chispa-test.layer.ext4")
	fi, err := os.Stat(layer)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= grow<<20 {
		t.Errorf("layer is %d bytes: not shrunk below GROW (%d MiB)", fi.Size(), grow)
	}
	if _, err := os.Stat(layer + ".shrink"); err == nil {
		t.Error("the shrink copy was left behind")
	}
	fsSano(t, layer, "after the build")
	if out := run(t, "dumpe2fs", "-h", layer); strings.Contains(out, "resize_inode") {
		t.Errorf("layer still has resize_inode:\n%s", out)
	}

	// Un refresco la agranda sin montarla (crecerImagen): tiene que seguir sana.
	if err := os.Truncate(layer, fi.Size()+64<<20); err != nil {
		t.Fatal(err)
	}
	run(t, "resize2fs", layer)
	fsSano(t, layer, "after growing it")
}

// fsSano falla si e2fsck encuentra algo. El código de salida de `e2fsck -fn`
// no basta: 1.47.0 sale con 0 tras contestar «no» a «Resize inode not valid.
// Recreate?», así que también cuenta cualquier pregunta sin arreglar.
func fsSano(t *testing.T, img, cuando string) {
	t.Helper()
	out, err := exec.Command("e2fsck", "-fn", img).CombinedOutput()
	if err != nil || strings.Contains(string(out), "? no\n") {
		t.Fatalf("e2fsck %s: %v\n%s", cuando, err, out)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustMkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeRandom(t *testing.T, path string, n int) {
	t.Helper()
	b := make([]byte, n)
	_, _ = rand.Read(b)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}
