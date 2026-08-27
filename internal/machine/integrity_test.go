package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// El digest se calcula sobre el contenido: mismos bytes, mismo hash; un byte
// distinto, hash distinto. Es la propiedad de la que cuelga toda la integridad.
func TestFileSHA256DetectaCambios(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "overlay.ext4")
	if err := os.WriteFile(f, []byte("contenido dorado"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := fileSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	// Releer el mismo fichero da el mismo digest.
	if h2, _ := fileSHA256(f); h2 != h1 {
		t.Fatalf("dos lecturas del mismo fichero dieron hashes distintos: %s vs %s", h1, h2)
	}
	// Cambiar un byte cambia el digest.
	if err := os.WriteFile(f, []byte("contenido doradO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h3, _ := fileSHA256(f); h3 == h1 {
		t.Fatal("el digest no cambió tras modificar el fichero")
	}
}

// verifyIntegrity acepta un snapshot cuyos ficheros coinciden con los digests
// grabados, y lo rechaza en cuanto uno se corrompe.
func TestVerifyIntegrity(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.ext4")
	snapFile := filepath.Join(dir, "snap.file")
	if err := os.WriteFile(overlay, []byte("rootfs dorado"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapFile, []byte("volcado de estado"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootfsSHA, _ := fileSHA256(overlay)
	snapSHA, _ := fileSHA256(snapFile)

	var m Manager // verifyIntegrity no toca el estado del Manager
	snap := &api.Snapshot{Name: "svc", RootfsSHA256: rootfsSHA, SnapSHA256: snapSHA}

	// Íntegro: pasa.
	if err := m.verifyIntegrity(snap, dir); err != nil {
		t.Fatalf("un snapshot íntegro no debería fallar: %v", err)
	}

	// Corromper el rootfs: debe fallar y nombrar el fichero.
	if err := os.WriteFile(overlay, []byte("rootfs CORRUPTO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.verifyIntegrity(snap, dir); err == nil {
		t.Fatal("un rootfs corrupto debería fallar la verificación")
	}
}

// Un snapshot anterior a esta comprobación no tiene digests: se salta en vez de
// fallar, o reimportar dejaría de ser opcional para todos los servicios ya
// existentes.
func TestVerifyIntegrityLegacySeSalta(t *testing.T) {
	dir := t.TempDir()
	// Ni siquiera existen los ficheros: si intentara hashearlos, fallaría al abrir.
	var m Manager
	snap := &api.Snapshot{Name: "legacy"} // sin RootfsSHA256 ni SnapSHA256
	if err := m.verifyIntegrity(snap, dir); err != nil {
		t.Fatalf("un snapshot legacy sin digests no debería fallar: %v", err)
	}
}

// El veredicto se recuerda, PERO la corrupción se sigue detectando.
//
// Es el par que importa: recordar sin invalidar convertiría la verificación en un
// sello que siempre dice que sí, que es peor que no verificar — daría confianza
// falsa. La huella (tamaño + fecha) es lo que separa un caso del otro.
func TestElVeredictoDeIntegridadSeRecuerdaPeroSeInvalida(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.ext4")
	snapFile := filepath.Join(dir, "snap.file")
	escribir := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escribir(overlay, "rootfs dorado")
	escribir(snapFile, "volcado")
	rootfsSHA, _ := fileSHA256(overlay)
	snapSHA, _ := fileSHA256(snapFile)

	m := &Manager{}
	snap := &api.Snapshot{Name: "svc", RootfsSHA256: rootfsSHA, SnapSHA256: snapSHA}

	// 1 · La primera verifica de verdad y anota el veredicto.
	if err := m.verifyIntegrity(snap, dir); err != nil {
		t.Fatalf("un dorado íntegro no debería fallar: %v", err)
	}
	if !m.integridadYaVista("svc", dir) {
		t.Fatal("tras verificar debería quedar anotado")
	}

	// 2 · Con un digest IMPOSIBLE de cumplir, sigue pasando: es la prueba de que
	//     de verdad se saltó el hasheo y no lo repitió por casualidad.
	snapImposible := &api.Snapshot{Name: "svc",
		RootfsSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		SnapSHA256:   snapSHA}
	if err := m.verifyIntegrity(snapImposible, dir); err != nil {
		t.Fatalf("el veredicto recordado debería evitar el rehasheo: %v", err)
	}

	// 3 · Si el fichero CAMBIA, la huella deja de casar y se vuelve a verificar —
	//     y entonces la corrupción se detecta.
	time.Sleep(10 * time.Millisecond) // que la fecha pueda diferir
	escribir(overlay, "rootfs CORROMPIDO")
	if m.integridadYaVista("svc", dir) {
		t.Fatal("al cambiar el fichero la huella no debería casar")
	}
	err := m.verifyIntegrity(snap, dir)
	if err == nil {
		t.Fatal("un dorado corrupto debe fallar aunque antes hubiera pasado")
	}
	if !strings.Contains(err.Error(), "overlay.ext4") {
		t.Errorf("el error debería nombrar el fichero corrupto: %v", err)
	}
}
