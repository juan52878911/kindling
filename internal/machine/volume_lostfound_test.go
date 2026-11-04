package machine

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Un volumen nuevo está vacío, como el de Docker: sin lost+found (initdb no
// inicializa un directorio con algo), ni después del e2fsck -f del arranque.
func TestVolumenNuevoSinLostFound(t *testing.T) {
	for _, b := range []string{"mkfs.ext4", "e2fsck", "debugfs"} {
		if buscarE2fs(b) == "" && !(b == "mkfs.ext4" && buscarE2fs("mke2fs") != "") {
			t.Skipf("no %s on this host", b)
		}
	}
	path := filepath.Join(t.TempDir(), "v.ext4")
	if err := createVolumeImage(t.Context(), path, 64); err != nil {
		t.Fatal(err)
	}
	ls := func() string {
		out, err := exec.Command(debugfsBin(), "-R", "ls -l /", path).CombinedOutput()
		if err != nil {
			t.Fatalf("debugfs: %v: %s", err, out)
		}
		return string(out)
	}
	if s := ls(); strings.Contains(s, "lost+found") {
		t.Fatalf("a new volume has lost+found:\n%s", s)
	}
	// El arranque (repairVolume) pasa e2fsck -f, que lo recrea; se quita otra
	// vez sin avisar de una reparación.
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	repairVolume(t.Context(), path)
	if s := ls(); strings.Contains(s, "lost+found") {
		t.Fatalf("lost+found is back after repairVolume:\n%s", s)
	}
	if logs.Len() > 0 {
		t.Errorf("repairVolume logged: %s", logs.String())
	}
	if reparado, out, err := revisarExt4(t.Context(), path); err != nil || !reparado {
		t.Fatalf("e2fsck after the removal: reparado=%v err=%v\n%s", reparado, err, out)
	} else if resto, ok := sinLostFoundCreado(out); !ok || !soloResumenE2fsck(resto) {
		t.Fatalf("e2fsck said more than recreating lost+found:\n%s", out)
	}
}

// Con datos en la raíz, el lost+found que recrea e2fsck se queda.
func TestRepairVolumenConDatosConservaLostFound(t *testing.T) {
	for _, b := range []string{"e2fsck", "debugfs"} {
		if buscarE2fs(b) == "" {
			t.Skipf("no %s on this host", b)
		}
	}
	if buscarE2fs("mkfs.ext4") == "" && buscarE2fs("mke2fs") == "" {
		t.Skip("no mke2fs on this host")
	}
	path := filepath.Join(t.TempDir(), "v.ext4")
	if err := createVolumeImage(t.Context(), path, 64); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(debugfsBin(), "-w", "-R", "mkdir PG_DATA", path).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repairVolume(t.Context(), path)
	out, _ := exec.Command(debugfsBin(), "-R", "ls -l /", path).CombinedOutput()
	if !strings.Contains(string(out), "lost+found") || !strings.Contains(string(out), "PG_DATA") {
		t.Fatalf("root after repairVolume:\n%s", out)
	}
}

// Con algo más en la raíz, o dentro de lost+found, no se toca.
func TestSoloLostFoundVacio(t *testing.T) {
	raiz := ` 2   40755 (2)      0      0    4096 3-Nov-2026 10:00 .
 2   40755 (2)      0      0    4096 3-Nov-2026 10:00 ..
 11   40700 (2)      0      0   16384 3-Nov-2026 10:00 lost+found
`
	lf := ` 11   40700 (2)      0      0   16384 3-Nov-2026 10:00 .
 2   40755 (2)      0      0    4096 3-Nov-2026 10:00 ..
 0       0 (0)      0      0       0
 0       0 (0)      0      0       0
`
	if !soloLostFoundVacio("debugfs 1.47.1 (20-May-2024)\n"+raiz, lf) {
		t.Error("a fresh volume should qualify")
	}
	if soloLostFoundVacio(raiz+" 12  100644 (1)  0  0  5 3-Nov-2026 10:00 PG_VERSION\n", lf) {
		t.Error("a root with data should not qualify")
	}
	if soloLostFoundVacio(raiz, lf+" 13  100600 (1)  0  0  0 3-Nov-2026 10:00 .kling-seeded\n") {
		t.Error("a lost+found with the seed mark should not qualify")
	}
	if soloLostFoundVacio("", "") {
		t.Error("empty debugfs output should not qualify")
	}
}
