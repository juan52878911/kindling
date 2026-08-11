package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// El nombre acaba siendo un componente de ruta: volumes/<nombre>.ext4. Si se
// cuela un ../, el volumen se crea o se borra fuera de su directorio.
func TestNombreDeVolumenRechazaTravesias(t *testing.T) {
	malos := []string{"../fuera", "a/b", "/absoluto", "", "CON-MAYUSCULAS",
		"con espacio", "punto.punto", strings.Repeat("x", 65), "-empieza-con-guion"}
	for _, n := range malos {
		if reVolume.MatchString(n) {
			t.Errorf("debería rechazar %q", n)
		}
	}
	for _, n := range []string{"notas", "mi-volumen", "vol_1", "a", strings.Repeat("x", 64)} {
		if !reVolume.MatchString(n) {
			t.Errorf("debería aceptar %q", n)
		}
	}
}

// El punto de montaje viaja en la línea de comandos del KERNEL, donde el
// separador es el espacio. Uno con espacios partiría el argumento y el invitado
// montaría en otro sitio, o en ninguno.
func TestPuntoDeMontajeRechazaLoQueRompeElCmdline(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("v"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	malos := []string{"con espacio", "rel/ativa", `con"comilla`, "con\ttab"}
	for _, mp := range malos {
		if _, _, err := m.resolveVolume(api.RunRequest{Volume: "v", VolumeMount: mp}); err == nil {
			t.Errorf("debería rechazar el punto de montaje %q", mp)
		}
	}
	// Y el caso bueno, con el defecto aplicado.
	p, mp, err := m.resolveVolume(api.RunRequest{Volume: "v"})
	if err != nil {
		t.Fatalf("un volumen existente sin -mount debería valer: %v", err)
	}
	if mp != "/data" {
		t.Errorf("punto de montaje por defecto = %q, want /data", mp)
	}
	if !strings.HasSuffix(p, "volumes/v.ext4") {
		t.Errorf("ruta inesperada: %s", p)
	}
}

// Pedir un volumen que no existe tiene que decir cómo crearlo, no fallar seco.
func TestVolumenInexistenteExplicaComoCrearlo(t *testing.T) {
	m := newTestManager(t)
	_, _, err := m.resolveVolume(api.RunRequest{Volume: "no-existe"})
	if err == nil {
		t.Fatal("debería fallar")
	}
	if !strings.Contains(err.Error(), "kling volume create") {
		t.Errorf("el error no dice cómo arreglarlo: %v", err)
	}
}

// No pedir volumen es el caso NORMAL, no un error.
func TestSinVolumenNoEsError(t *testing.T) {
	m := newTestManager(t)
	p, mp, err := m.resolveVolume(api.RunRequest{})
	if err != nil || p != "" || mp != "" {
		t.Errorf("sin volumen debería devolver vacío y nil: %q %q %v", p, mp, err)
	}
}

// El parámetro del kernel solo aparece si hay volumen: una línea de comandos con
// kling.volume= vacío haría que el puente intentara montar en "".
func TestArgDeArranqueSoloConVolumen(t *testing.T) {
	if got := volumeBootArg(""); got != "" {
		t.Errorf("sin volumen no debe añadir nada, añadió %q", got)
	}
	got := volumeBootArg("/data")
	if !strings.Contains(got, api.VolumeBootParam+"=/data") {
		t.Errorf("falta el parámetro: %q", got)
	}
	// Y tiene que ir separado del resto de la línea.
	if !strings.HasPrefix(got, " ") {
		t.Errorf("se pegaría al argumento anterior: %q", got)
	}
	full := bootArgs("/data")
	if !strings.Contains(full, "root=/dev/vda") || !strings.Contains(full, api.VolumeBootParam+"=/data") {
		t.Errorf("la línea de comandos perdió algo: %s", full)
	}
}

// Borrar un volumen que una microVM tiene montado le corrompe el sistema de
// ficheros por debajo. Esta comprobación es lo único que lo impide.
func TestNoSeBorraUnVolumenEnUso(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("notas"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateRunning, Volume: "notas"}
	m.mu.Unlock()

	err := m.RemoveVolume("notas")
	if err == nil {
		t.Fatal("borró un volumen en uso")
	}
	if !strings.Contains(err.Error(), "svc-a") {
		t.Errorf("el error debería decir QUIÉN lo usa: %v", err)
	}
	if _, sErr := os.Stat(m.volumePath("notas")); sErr != nil {
		t.Error("el fichero se borró pese al error")
	}

	// Una máquina parada no lo retiene: su volumen ya no está montado.
	m.mu.Lock()
	m.byID["a"].State = api.StateStopped
	m.mu.Unlock()
	if err := m.RemoveVolume("notas"); err != nil {
		t.Errorf("con la máquina parada debería poder borrarse: %v", err)
	}
}

// Volumes() informa de quién lo usa, que es lo que hace comprensible el rechazo.
func TestVolumesInformaDeQuienLoUsa(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"uno", "dos"} {
		if err := os.WriteFile(m.volumePath(n), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateRunning, Volume: "uno"}
	m.byID["b"] = &api.Machine{ID: "b", Name: "svc-b", State: api.StateWarm, Volume: "uno"}
	m.mu.Unlock()

	vols := m.Volumes()
	if len(vols) != 2 {
		t.Fatalf("esperaba 2 volúmenes, hay %d", len(vols))
	}
	byName := map[string]*api.Volume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	if n := len(byName["uno"].UsedBy); n != 2 {
		t.Errorf("\"uno\" lo usan 2 máquinas, informó %d", n)
	}
	if n := len(byName["dos"].UsedBy); n != 0 {
		t.Errorf("\"dos\" no lo usa nadie, informó %d", n)
	}
	// Una máquina warm SÍ retiene el volumen: al descongelar vuelve a montarlo.
	if !strings.Contains(strings.Join(byName["uno"].UsedBy, ","), "svc-b") {
		t.Error("una máquina warm debería seguir contando como usuaria")
	}
}

// Crear dos veces el mismo volumen no debe pisar el primero: dentro hay datos.
func TestCrearVolumenNoPisaElExistente(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("notas"), []byte("datos importantes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateVolume(t.Context(), "notas", 64); err == nil {
		t.Fatal("debería negarse: ya existe")
	}
	b, _ := os.ReadFile(m.volumePath("notas"))
	if string(b) != "datos importantes" {
		t.Error("pisó el contenido de un volumen existente")
	}
	// Y no debe dejar un .tmp por ahí.
	if _, err := os.Stat(m.volumePath("notas") + ".tmp"); err == nil {
		t.Error("quedó un .tmp suelto")
	}
	_ = filepath.Join // silencia el import si el resto cambia
}

// Dos microVMs no pueden montar el mismo volumen a la vez.
//
// No es una política: un ext4 no admite dos escritores. Cada uno cachea
// metadatos que el otro no ve y el sistema de ficheros se corrompe. Comprobado
// en el laboratorio — el síntoma es un EBADMSG al leer desde la siguiente
// máquina, que no señala a la causa por ninguna parte.
func TestUnVolumenNoSeMontaDosVeces(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("notas"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Libre: se puede montar.
	if _, _, err := m.resolveVolume(api.RunRequest{Volume: "notas"}); err != nil {
		t.Fatalf("un volumen libre debería poder montarse: %v", err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "primera", State: api.StateRunning, Volume: "notas"}
	m.mu.Unlock()

	_, _, err := m.resolveVolume(api.RunRequest{Volume: "notas"})
	if err == nil {
		t.Fatal("dejó montar el mismo volumen en dos máquinas a la vez")
	}
	if !strings.Contains(err.Error(), "primera") {
		t.Errorf("el error debe decir QUIÉN lo tiene: %v", err)
	}

	// Una máquina warm también lo retiene: al descongelar vuelve a montarlo.
	m.mu.Lock()
	m.byID["a"].State = api.StateWarm
	m.mu.Unlock()
	if _, _, err := m.resolveVolume(api.RunRequest{Volume: "notas"}); err == nil {
		t.Error("una máquina warm sigue siendo dueña del volumen")
	}

	// Parada, ya no.
	m.mu.Lock()
	m.byID["a"].State = api.StateStopped
	m.mu.Unlock()
	if _, _, err := m.resolveVolume(api.RunRequest{Volume: "notas"}); err != nil {
		t.Errorf("con la máquina parada debería liberarse: %v", err)
	}
}

// Un snapshot tiene que RECORDAR su volumen.
//
// El conjunto de discos de una microVM queda fijado al congelarla, así que si el
// snapshot no recuerda con qué volumen se importó, el gateway la despierta sin
// él y la herramienta escribe en un overlay que muere con la máquina. Sin un
// solo error: la escritura dice "success", y el fichero no está la próxima vez.
//
// Eso fue exactamente lo que pasó en el laboratorio, y por eso este test existe.
func TestElSnapshotRecuerdaSuVolumen(t *testing.T) {
	mc := &api.Machine{Volume: "notas", VolumeMount: "/data", Egress: "internet"}
	snap := &api.Snapshot{
		Egress:      mc.Egress,
		Volume:      mc.Volume,
		VolumeMount: mc.VolumeMount,
		HasVolume:   mc.Volume != "",
	}
	if !snap.HasVolume || snap.Volume != "notas" || snap.VolumeMount != "/data" {
		t.Fatalf("el snapshot perdió el volumen: %+v", snap)
	}

	// Y al despertar, una petición SIN volumen tiene que heredarlo: el gateway
	// despierta servicios por nombre y no sabe nada de volúmenes.
	req := api.RunRequest{From: "svc"}
	if req.Volume == "" && snap.Volume != "" {
		req.Volume, req.VolumeMount = snap.Volume, snap.VolumeMount
	}
	if req.Volume != "notas" || req.VolumeMount != "/data" {
		t.Errorf("no heredó el volumen del snapshot: %+v", req)
	}

	// Una máquina sin volumen no debe marcar el snapshot como si lo tuviera:
	// eso obligaría a pasar -volume para despertar servicios que no lo usan.
	sin := &api.Snapshot{HasVolume: (&api.Machine{}).Volume != ""}
	if sin.HasVolume {
		t.Error("marcó HasVolume en una máquina sin volumen")
	}
}
