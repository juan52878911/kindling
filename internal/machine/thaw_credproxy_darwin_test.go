//go:build darwin

package machine

// El thaw de una máquina con credenciales en macOS con el proxy aparte: el
// proxy se lanza en cuanto existe el VMM, una vez; la entrega no se repite
// tras cargar; un fallo al entregar no aborta el thaw; y el proxy solo se
// lanza junto al VMM del daemon.

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// maquinaCongelada deja en m la máquina id congelada (volcado sellado) con
// egress allowlist y las credenciales creds.
func maquinaCongelada(t *testing.T, m *Manager, id string, creds []credproxy.Credential) {
	t.Helper()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := m.guardarCredenciales(id, creds); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-" + id[:4], State: api.StateWarm, Egress: "allowlist",
		AllowDomains: []string{"example.org"}, IP: knet.GuestIP, CreatedAt: time.Now()}
}

func contar(ls []string, pre string) int { return len(lineasCon(ls, pre)) }

// P1: en un thaw, un solo proxy y una sola entrega (antes de cargar); después
// de cargar, solo los marcadores. Antes la entrega entera se repetía, y el
// tiempo de entregar iba dentro de net.
func TestThawConProxyAparteEntregaUnaVez(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t)
	id := "dd11ee22ff33aa44"
	maquinaCongelada(t, m, id, []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-aa", Secret: "sk-clave-de-prueba"}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	ls := llamadas(t, logPath)
	load := indice(ls, "PUT /snapshot/load")
	if load < 0 {
		t.Fatalf("no cargó:\n%s", strings.Join(ls, "\n"))
	}
	if n := contar(ls, "CP START"); n != 1 {
		t.Fatalf("%d proxies lanzados, quería 1:\n%s", n, strings.Join(ls, "\n"))
	}
	if !strings.Contains(lineasCon(ls, "CP START")[0], "vmm="+strconv.Itoa(out.PID)+" ") {
		t.Fatalf("el proxy no vigila al VMM del thaw (pid %d): %q", out.PID, lineasCon(ls, "CP START"))
	}
	if n := contar(ls, "CP PUT /kling/credentials"); n != 1 {
		t.Fatalf("%d entregas al proxy, quería 1:\n%s", n, strings.Join(ls, "\n"))
	}
	if n, i := contar(ls, "PUT /kling/credproxy"), indice(ls, "PUT /kling/credproxy"); n != 1 || i > load {
		t.Fatalf("el VMM se apuntó al proxy %d veces (la primera en %d, la carga en %d), quería una antes de cargar:\n%s",
			n, i, load, strings.Join(ls, "\n"))
	}
	if indice(ls[load:], "PATCH /mmds") < 0 {
		t.Fatalf("tras cargar no volvieron los marcadores a MMDS:\n%s", strings.Join(ls, "\n"))
	}
	if out.Wake == nil || out.Wake.CredsMS <= 0 {
		t.Fatalf("la entrega no tiene su fase: %+v", out.Wake)
	}
}

// P1: el lanzamiento adelantado no necesita el socket del VMM (el PID es el
// del hijo recién lanzado), y la entrega de después lo encuentra corriendo.
func TestPrecalentarCredproxySinElSocketDelVMM(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t)
	id := "dd11ee22ff33aa45"
	vmm := arrancarVZ(t, m, id, "allowlist")
	creds := []credproxy.Credential{{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"}}
	if err := m.guardarCredenciales(id, creds); err != nil {
		t.Fatal(err)
	}
	antes := len(llamadas(t, logPath))
	m.precalentarCredproxy(id, vmm)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	esperarCredproxyEnCurso(ctx, filepath.Dir(m.credAuditPath(id)))
	nuevas := llamadas(t, logPath)[antes:]
	if contar(nuevas, "CP START") != 1 || !strings.Contains(nuevas[0], "vmm="+strconv.Itoa(vmm)+" ") {
		t.Fatalf("el lanzamiento adelantado no lanzó el proxy de este VMM: %q", nuevas)
	}
	for _, l := range nuevas {
		if !strings.HasPrefix(l, "CP ") {
			t.Fatalf("el lanzamiento adelantado habló con el VMM (%q): %q", l, nuevas)
		}
	}
	if err := registrarCredencialesPlataforma(m.ctxCredenciales(ctx), fc.New(m.socket[id]), nil, creds, m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	ls := llamadas(t, logPath)
	if n := contar(ls, "CP START"); n != 1 {
		t.Fatalf("%d proxies lanzados, quería 1: %q", n, ls)
	}
	if contar(ls, "CP PUT /kling/credentials n=1 api.example.com+secret") != 1 {
		t.Fatalf("el proxy adelantado no recibió las credenciales: %q", ls)
	}
}

// F5: un -upstream que no resuelve (un portátil sin red) no impide
// descongelar la máquina: despierta sin credenciales, sin que el VMM desvíe
// el dominio a ningún proxy, y queda dicho. Antes el thaw abortaba.
func TestThawSinPoderEntregarLasCredencialesNoAborta(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,postgres,postgres-upstream,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	prev := resolverUpstream
	t.Cleanup(func() { resolverUpstream = prev })
	resolverUpstream = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no such host")
	}
	m, logPath := managerVZ(t)
	id := "dd11ee22ff33aa46"
	maquinaCongelada(t, m, id, []credproxy.Credential{
		{Env: "PGPASSWORD", Domain: "db.example.com", Placeholder: "kling-cred-cc", Secret: "pw-clave-de-prueba",
			Kind: credproxy.KindPostgres, Port: 5432, User: "app", Database: "app", Upstream: "db.sin-red.example:5432"}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("el thaw abortó por no poder entregar las credenciales: %v", err)
	}
	if out.State != api.StateRunning {
		t.Fatalf("estado tras el thaw: %s", out.State)
	}
	ls := llamadas(t, logPath)
	if contar(ls, "CP PUT /kling/credentials") != 0 || contar(ls, "PUT /kling/credproxy") != 0 {
		t.Fatalf("algo recibió las credenciales que no se pudieron resolver:\n%s", strings.Join(ls, "\n"))
	}
}

// S9: el proxy aparte solo se lanza junto al VMM del daemon. Si quien
// contesta en el socket de la máquina es otro ejecutable, no se lanza nada
// desde su directorio ni se le da nada.
func TestCredproxySoloJuntoAlVMMDelDaemon(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t)
	id := "dd11ee22ff33aa47"
	arrancarVZ(t, m, id, "allowlist")
	// El VMM que contesta es este binario de test; el del daemon, otro.
	m.mu.Lock()
	m.fcBin = "/usr/local/bin/kling-hv"
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	creds := []credproxy.Credential{{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-dd", Secret: "sk-clave-de-prueba"}}
	err := registrarCredencialesPlataforma(m.ctxCredenciales(ctx), fc.New(m.socket[id]), nil, creds, m.credAuditPath(id), nil, "allowlist")
	if err == nil || !strings.Contains(err.Error(), "not the daemon's VMM") {
		t.Fatalf("las credenciales fueron a un VMM que no es el del daemon: %v", err)
	}
	if ls := llamadas(t, logPath); contar(ls, "CP ") != 0 {
		t.Fatalf("se lanzó un proxy junto a un ejecutable ajeno: %q", ls)
	}
}
