package imagen

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/guest"
)

// El init en Go hace sin sh lo que hace el /entrypoint generado sin
// servicio: tiene que reconocer exactamente lo que escribe Entrypoint.
func TestEntrypointLoReconoceElInitEnGo(t *testing.T) {
	for _, m := range []Marca{marcaPrueba, {Constructor: "oci", Dir: "/etc/kindling", DM: "kindling-layer"}} {
		if env, ok := guest.PlainEntrypoint([]byte(Entrypoint(m, []string{"A=b"}, ""))); !ok || !env {
			t.Fatalf("%s with env: %v %v", m.Constructor, ok, env)
		}
		if env, ok := guest.PlainEntrypoint([]byte(Entrypoint(m, nil, ""))); !ok || env {
			t.Fatalf("%s without env: %v %v", m.Constructor, ok, env)
		}
		if _, ok := guest.PlainEntrypoint([]byte(Entrypoint(m, nil, "/usr/bin/svc"))); ok {
			t.Fatalf("%s: one with a service is not plain", m.Constructor)
		}
	}
	if marcaPrueba.VerityConf() != guest.VerityConfPath || marcaPrueba.DM != guest.VerityDM {
		t.Fatal("the debian Marca and the Go init's verity table drifted apart")
	}
}

var marcaPrueba = Marca{Constructor: "debian", Dir: "/etc/kindling", DM: "kindling-layer"}

func agentePrueba(t *testing.T, marca bool) string {
	b := make([]byte, 64)
	copy(b, "\x7fELF\x02\x01\x01")
	b[18] = 0x3e
	s := string(b)
	if marca {
		s += guest.InitMarker
	}
	p := filepath.Join(t.TempDir(), "kling-guest")
	if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func escribirBase(t *testing.T, d Debs, table string) *ext4.Node {
	tm := time.Unix(1700000000, 0)
	d.T = tm
	b := &Base{Root: ext4.NewDir(0o755, 0, 0, tm), debs: d, arch: "amd64"}
	dst := filepath.Join(t.TempDir(), "base.ext4")
	if _, err := b.Write(dst, [16]byte{1}, table); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func leer(t *testing.T, r *ext4.Node, p string) string {
	n := r.Lookup(p)
	if n == nil || !n.IsReg() {
		return ""
	}
	rc, err := n.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// Con un agente que sabe hacer de init, la base lo lleva y overlay-init es
// un enlace a él, también con verity (la tabla, donde la busca). Sin él, o
// con uno anterior, o con una Marca cuya tabla el init en Go no conoce, el
// script de siempre.
func TestBaseWriteInitEnGo(t *testing.T) {
	const tabla = "0 8 verity 1 @DEV@ @DEV@ 4096 4096 1 2 sha256 aa bb"
	r := escribirBase(t, Debs{Marca: marcaPrueba, Agent: agentePrueba(t, true)}, tabla)
	if n := r.Lookup("/usr/sbin/overlay-init"); n == nil || !n.IsLink() || n.Target != "/usr/local/bin/kling-guest" {
		t.Fatalf("overlay-init: %+v", n)
	}
	if !strings.Contains(leer(t, r, "/usr/local/bin/kling-guest"), guest.InitMarker) {
		t.Fatal("the agent is not in the base")
	}
	if !strings.Contains(leer(t, r, guest.VerityConfPath), tabla) {
		t.Fatal("verity table")
	}

	for name, d := range map[string]Debs{
		"no agent":    {Marca: marcaPrueba},
		"old agent":   {Marca: marcaPrueba, Agent: agentePrueba(t, false)},
		"other marca": {Marca: Marca{Constructor: "android", Dir: "/etc/kindling-android", DM: "android-layer"}, Agent: agentePrueba(t, true)},
	} {
		r := escribirBase(t, d, tabla)
		init := leer(t, r, "/usr/sbin/overlay-init")
		if !strings.Contains(init, "dmsetup create "+d.Marca.DM) || r.Lookup("/usr/local/bin/kling-guest") != nil {
			t.Fatalf("%s: overlay-init:\n%.200s", name, init)
		}
	}
	// Una Marca que no es la del init en Go sin verity: el de Go vale.
	r = escribirBase(t, Debs{Marca: Marca{Constructor: "android", Dir: "/etc/kindling-android", DM: "android-layer"}, Agent: agentePrueba(t, true)}, "")
	if n := r.Lookup("/usr/sbin/overlay-init"); n == nil || !n.IsLink() {
		t.Fatal("without verity the Go init does not depend on the Marca")
	}
}
