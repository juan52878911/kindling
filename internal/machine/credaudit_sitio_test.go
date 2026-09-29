package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// conAuditoriaEnElDaemon fuerza el camino de Linux (registro en <root>/audit)
// también en macOS, y lo deja como estaba al acabar.
func conAuditoriaEnElDaemon(t *testing.T) {
	t.Helper()
	antes := auditoriaEnElDaemon
	auditoriaEnElDaemon = true
	t.Cleanup(func() { auditoriaEnElDaemon = antes })
}

func lineaAuditoria(ruta string) string {
	return `{"ts":"2026-09-28T10:00:00Z","kind":"http","method":"GET","host":"a.com","path":"` + ruta + `","status":200}` + "\n"
}

// En Linux el registro vive fuera del directorio de la máquina (que es del
// VMM), en un directorio 0700 del daemon.
func TestAuditoriaFueraDelDirectorioDeLaMaquina(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	p := m.credAuditPath("m1")
	if p != filepath.Join(m.root, "audit", "m1.jsonl") {
		t.Fatalf("ruta = %s", p)
	}
	if strings.HasPrefix(p, m.dir("m1")+string(os.PathSeparator)) {
		t.Fatalf("el registro sigue en el directorio de la máquina: %s", p)
	}
}

// Al arrancar, el daemon lleva los registros viejos (y su rotación) a
// <root>/audit, 0600, y los borra del directorio de la máquina; kling machine
// audit los sigue viendo.
func TestPrepararAuditoriaMigraLosRegistrosViejos(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	m.addForTest("m1")
	if err := os.Chmod(m.dirAuditoria(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	viejo := filepath.Join(m.dir("m1"), credproxy.AuditFile)
	if err := os.WriteFile(viejo+".1", []byte(lineaAuditoria("/antes")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(viejo, []byte(lineaAuditoria("/ahora")), 0o600); err != nil {
		t.Fatal(err)
	}

	m.prepararAuditoria()

	if modo(t, m.dirAuditoria()) != 0o700 {
		t.Errorf("audit/ = %o, quería 0700", modo(t, m.dirAuditoria()))
	}
	for _, f := range []string{viejo, viejo + ".1"} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s sigue en el directorio de la máquina: %v", f, err)
		}
	}
	for _, f := range []string{m.credAuditPath("m1"), m.credAuditPath("m1") + ".1"} {
		if modo(t, f) != 0o600 {
			t.Errorf("%s = %o, quería 0600", f, modo(t, f))
		}
	}
	got, err := m.CredAudit("m1", api.CredAuditQuery{})
	if err != nil || len(got) != 2 || got[0].Path != "/antes" || got[1].Path != "/ahora" {
		t.Fatalf("tras migrar: %+v %v", got, err)
	}

	// Un segundo arranque no toca nada.
	m.prepararAuditoria()
	if got, _ := m.CredAudit("m1", api.CredAuditQuery{}); len(got) != 2 {
		t.Fatalf("segundo arranque: %+v", got)
	}
}

// Lo que un VMM comprometido deje en el sitio del registro viejo (un enlace a
// otro fichero, un enlace duro) no se lee ni se copia: se descarta.
func TestPrepararAuditoriaDescartaEnlacesPlantados(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	m.addForTest("m1")
	m.addForTest("m2")
	fuera := t.TempDir()
	ajeno := filepath.Join(fuera, "ajeno")
	if err := os.WriteFile(ajeno, []byte(lineaAuditoria("/secreto")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2"} {
		if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	simb := filepath.Join(m.dir("m1"), credproxy.AuditFile)
	duro := filepath.Join(m.dir("m2"), credproxy.AuditFile)
	if err := os.Symlink(ajeno, simb); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(ajeno, duro); err != nil {
		t.Skipf("sin enlaces duros aquí: %v", err)
	}

	m.prepararAuditoria()

	for _, id := range []string{"m1", "m2"} {
		if _, err := os.Lstat(m.credAuditPath(id)); !os.IsNotExist(err) {
			t.Errorf("%s: se copió lo plantado: %v", id, err)
		}
	}
	for _, f := range []string{simb, duro} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s no se descartó: %v", f, err)
		}
	}
	if b, err := os.ReadFile(ajeno); err != nil || string(b) != lineaAuditoria("/secreto") {
		t.Fatalf("el fichero ajeno cambió: %q %v", b, err)
	}
}

// Si ya hay registro nuevo, manda ese: el viejo se descarta sin mezclarlo.
func TestPrepararAuditoriaNoPisaElNuevo(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	m.addForTest("m1")
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	viejo := filepath.Join(m.dir("m1"), credproxy.AuditFile)
	if err := os.WriteFile(viejo, []byte(lineaAuditoria("/viejo")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.credAuditPath("m1"), []byte(lineaAuditoria("/nuevo")), 0o600); err != nil {
		t.Fatal(err)
	}
	m.prepararAuditoria()
	got, err := m.CredAudit("m1", api.CredAuditQuery{})
	if err != nil || len(got) != 1 || got[0].Path != "/nuevo" {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := os.Lstat(viejo); !os.IsNotExist(err) {
		t.Errorf("el viejo sigue: %v", err)
	}
}

// Los registros de máquinas que ya no existen se barren al arrancar; lo que no
// tiene forma de registro se deja.
func TestPrepararAuditoriaBarreHuerfanos(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	m.addForTest("m1")
	for _, f := range []string{"m1.jsonl", "m1.jsonl.1", "zz.jsonl", "zz.jsonl.1", "notas"} {
		if err := os.WriteFile(filepath.Join(m.dirAuditoria(), f), []byte(lineaAuditoria("/x")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.prepararAuditoria()
	for f, quiere := range map[string]bool{"m1.jsonl": true, "m1.jsonl.1": true, "zz.jsonl": false, "zz.jsonl.1": false, "notas": true} {
		_, err := os.Lstat(filepath.Join(m.dirAuditoria(), f))
		if (err == nil) != quiere {
			t.Errorf("%s: existe=%v, quería %v", f, err == nil, quiere)
		}
	}
}

// Un enlace en el sitio de audit/ no se usa: el directorio tiene que ser de
// verdad.
func TestAsegurarDirPrivadoRechazaUnEnlace(t *testing.T) {
	root := t.TempDir()
	otro := t.TempDir()
	if err := os.Symlink(otro, filepath.Join(root, "audit")); err != nil {
		t.Fatal(err)
	}
	if err := asegurarDirPrivado(filepath.Join(root, "audit")); err == nil {
		t.Fatal("aceptó un enlace como directorio de auditoría")
	}
}

// rm borra el registro de la máquina (y su rotación) de <root>/audit.
func TestRemoveBorraElRegistroDeAuditoria(t *testing.T) {
	conAuditoriaEnElDaemon(t)
	m := newTestManager(t)
	m.bus = events.New()
	mc := m.addForTest(newID())
	p := m.credAuditPath(mc.ID)
	for _, f := range []string{p, p + ".1"} {
		if err := os.WriteFile(f, []byte(lineaAuditoria("/x")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Remove(mc.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, p + ".1"} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s sigue tras rm: %v", f, err)
		}
	}
}
