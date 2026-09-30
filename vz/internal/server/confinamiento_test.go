package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Al confinarse, kling-vz pide solo los ficheros de SU VM: el kernel y los
// discos de solo lectura para leer, los de lectura y escritura para escribir.
// Nada de la raíz entera (secrets/, los dorados y las máquinas de otros).
func TestConfinamientoSoloLoDeLaVM(t *testing.T) {
	r := newRig(t)
	var got []Confinamiento
	r.srv.d.Confine = func(c Confinamiento) error { got = append(got, c); return nil }
	dir := t.TempDir()
	r.configure(dir)
	r.must("PUT", "/kling/network", `{"egress":"allowlist","allow_domains":["a.com"]}`)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if len(got) != 1 {
		t.Fatalf("Confine llamado %d veces", len(got))
	}
	c := got[0]
	slices.Sort(c.Lectura)
	if want := []string{"/i/min.ext4", "/i/x.layer.ext4", "/k/vmlinux"}; !slices.Equal(c.Lectura, want) {
		t.Fatalf("Lectura = %v, want %v", c.Lectura, want)
	}
	if want := []string{dir + "/overlay.ext4"}; !slices.Equal(c.Escritura, want) {
		t.Fatalf("Escritura = %v, want %v", c.Escritura, want)
	}
	if !c.ConRed || !c.Loopback {
		t.Fatalf("allowlist: ConRed y Loopback, got %+v", c)
	}
}

// Al restaurar, además el estado del dorado (solo lectura), y los discos
// son los ya reapuntados: el overlay de la instancia, no el del dorado.
// Sin red no hay loopback.
func TestConfinamientoAlRestaurar(t *testing.T) {
	r := newRig(t)
	var got []Confinamiento
	r.srv.d.Confine = func(c Confinamiento) error { got = append(got, c); return nil }
	snap, mem := writeSnapshot(t, t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":false}`)
	r.must("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"/m/1/overlay.ext4"}`)
	r.must("PATCH", "/vm", `{"state":"Resumed"}`)
	if len(got) != 1 {
		t.Fatalf("Confine llamado %d veces", len(got))
	}
	c := got[0]
	if !slices.Contains(c.Lectura, mem) || !slices.Contains(c.Lectura, "/k") {
		t.Fatalf("Lectura = %v: faltan el estado o el kernel", c.Lectura)
	}
	if !slices.Equal(c.Escritura, []string{"/m/1/overlay.ext4"}) {
		t.Fatalf("Escritura = %v: el overlay del dorado no se escribe", c.Escritura)
	}
	if c.ConRed || c.Loopback {
		t.Fatalf("none: sin red ni loopback, got %+v", c)
	}
}

// Un snapshot con destino fuera del directorio de la máquina (kling commit)
// se vuelca a un temporal y se publica; los temporales no se quedan.
func TestSnapshotPorDestino(t *testing.T) {
	r := newRig(t)
	mdir, fuera := t.TempDir(), t.TempDir()
	var publicados []string
	r.srv.d.Destino = func(p string) (string, func() error, error) {
		if filepath.Dir(p) == mdir {
			return p, nil, nil
		}
		tmp := filepath.Join(mdir, ".tmp-"+filepath.Base(p))
		return tmp, func() error {
			b, err := os.ReadFile(tmp)
			if err != nil {
				return err
			}
			publicados = append(publicados, filepath.Base(p))
			return os.WriteFile(p, b, 0o600)
		}, nil
	}
	r.configure(mdir)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	snap, mem := filepath.Join(fuera, "snap.file"), filepath.Join(fuera, "mem.file")
	r.must("PUT", "/snapshot/create", `{"snapshot_path":"`+snap+`","mem_file_path":"`+mem+`"}`)
	if b, err := os.ReadFile(mem); err != nil || string(b) != "state" {
		t.Fatalf("mem.file en el destino: %q %v", b, err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatal(err)
	}
	if strings.Join(publicados, ",") != "mem.file,snap.file" {
		t.Fatalf("publicados = %v", publicados)
	}
	ents, _ := os.ReadDir(mdir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temporal %s olvidado en el directorio de la máquina", e.Name())
		}
	}

	// Si publicar falla, el snapshot falla (el daemon no da por bueno el
	// dorado) y tampoco quedan temporales.
	r.srv.d.Destino = func(p string) (string, func() error, error) {
		return filepath.Join(mdir, ".tmp-"+filepath.Base(p)), func() error { return errors.New("custodio: no") }, nil
	}
	otro := t.TempDir()
	r.mustFail("PUT", "/snapshot/create", `{"snapshot_path":"`+otro+`/snap.file","mem_file_path":"`+otro+`/mem.file"}`, "custodio: no")
	ents, _ = os.ReadDir(mdir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temporal %s olvidado tras un fallo", e.Name())
		}
	}
}
