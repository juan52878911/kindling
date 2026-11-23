package server

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
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

// Los reenvíos pedidos antes de crear la VM se abren ya, en el rango
// reservado y mientras kling-vz aún puede escuchar; la red los adopta con la
// misma dirección, y el confinamiento lo sabe (ReenviosAbiertos: el perfil no
// deja escuchar en TCP). Sin pedirlos antes, como con un núcleo anterior,
// ReenviosAbiertos queda a false y el perfil sigue dejando escuchar.
func TestReenviosAntesDeCrear(t *testing.T) {
	direcciones := func(out string) map[string]string {
		var fw struct {
			Forwards map[string]string `json:"forwards"`
		}
		if err := json.Unmarshal([]byte(out), &fw); err != nil {
			t.Fatalf("forwards = %s: %v", out, err)
		}
		return fw.Forwards
	}
	r := newRig(t)
	var got []Confinamiento
	r.srv.d.Confine = func(c Confinamiento) error { got = append(got, c); return nil }
	r.configure(t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	antes := direcciones(r.must("PUT", "/kling/forwards", `{"ports":[8080, 22]}`))
	for p, a := range antes {
		_, port, _ := net.SplitHostPort(a)
		if n, err := strconv.Atoi(port); err != nil || !credproxy.PuertoReservado(n) {
			t.Fatalf("reenvío %s en %s, fuera del rango reservado", p, a)
		}
		c, err := net.Dial("tcp4", a)
		if err != nil {
			t.Fatalf("el reenvío reservado %s no escucha: %v", a, err)
		}
		c.Close()
	}
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if len(got) != 1 || !got[0].ReenviosAbiertos {
		t.Fatalf("Confine = %+v, quería ReenviosAbiertos", got)
	}
	n := r.nets[len(r.nets)-1]
	t.Cleanup(func() {
		for _, l := range n.adoptados {
			l.Close()
		}
	})
	if len(n.adoptados) != 2 {
		t.Fatalf("la red adoptó %d reenvíos, quería 2", len(n.adoptados))
	}
	despues := direcciones(r.must("PUT", "/kling/forwards", `{"ports":[8080, 22]}`))
	if despues["8080"] != antes["8080"] || despues["22"] != antes["22"] {
		t.Fatalf("tras arrancar, %v; antes %v: deben ser los mismos", despues, antes)
	}

	r2 := newRig(t)
	var got2 []Confinamiento
	r2.srv.d.Confine = func(c Confinamiento) error { got2 = append(got2, c); return nil }
	r2.configure(t.TempDir())
	r2.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if len(got2) != 1 || got2[0].ReenviosAbiertos {
		t.Fatalf("sin reenvíos previos, Confine = %+v: ReenviosAbiertos debe ser false", got2)
	}
}
