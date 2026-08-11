package machine

import (
	"fmt"
	"os"
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
		if _, err := m.resolveVolumes(api.RunRequest{Volume: "v", VolumeMount: mp}); err == nil {
			t.Errorf("debería rechazar el punto de montaje %q", mp)
		}
	}
	// Y el caso bueno, con el defecto aplicado.
	got, err := m.resolveVolumes(api.RunRequest{Volume: "v"})
	if err != nil {
		t.Fatalf("un volumen existente sin -mount debería valer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("esperaba 1 volumen, hay %d", len(got))
	}
	if got[0].mount != "/data" {
		t.Errorf("punto de montaje por defecto = %q, want /data", got[0].mount)
	}
	if !strings.HasSuffix(got[0].path, "volumes/v.ext4") {
		t.Errorf("ruta inesperada: %s", got[0].path)
	}
}

// Pedir un volumen que no existe tiene que decir cómo crearlo, no fallar seco.
func TestVolumenInexistenteExplicaComoCrearlo(t *testing.T) {
	m := newTestManager(t)
	_, err := m.resolveVolumes(api.RunRequest{Volume: "no-existe"})
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
	got, err := m.resolveVolumes(api.RunRequest{})
	if err != nil || len(got) != 0 {
		t.Errorf("sin volumen debería devolver vacío y nil: %+v %v", got, err)
	}
}

// El parámetro del kernel solo aparece si hay volumen: una línea de comandos con
// kling.volume= vacío haría que el puente intentara montar en "".
func TestArgDeArranqueSoloConVolumen(t *testing.T) {
	if got := volumeBootArg(nil); got != "" {
		t.Errorf("sin volumen no debe añadir nada, añadió %q", got)
	}
	got := volumeBootArg([]api.VolumeAttachment{{Mount: "/data"}})
	if !strings.Contains(got, api.VolumeBootParam+"=/data") {
		t.Errorf("falta el parámetro: %q", got)
	}
	// Y tiene que ir separado del resto de la línea.
	if !strings.HasPrefix(got, " ") {
		t.Errorf("se pegaría al argumento anterior: %q", got)
	}
	full := bootArgs([]api.VolumeAttachment{{Mount: "/data"}}, false)
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
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateRunning,
		Volumes: []api.VolumeAttachment{{Name: "notas", Mount: "/data"}}}
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
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateRunning,
		Volumes: []api.VolumeAttachment{{Name: "uno", Mount: "/data"}}}
	m.byID["b"] = &api.Machine{ID: "b", Name: "svc-b", State: api.StateWarm,
		Volumes: []api.VolumeAttachment{{Name: "uno", Mount: "/data"}}}
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
	if _, err := m.resolveVolumes(api.RunRequest{Volume: "notas"}); err != nil {
		t.Fatalf("un volumen libre debería poder montarse: %v", err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "primera", State: api.StateRunning,
		Volumes: []api.VolumeAttachment{{Name: "notas", Mount: "/data"}}}
	m.mu.Unlock()

	_, err := m.resolveVolumes(api.RunRequest{Volume: "notas"})
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
	if _, err := m.resolveVolumes(api.RunRequest{Volume: "notas"}); err == nil {
		t.Error("una máquina warm sigue siendo dueña del volumen")
	}

	// Parada, ya no.
	m.mu.Lock()
	m.byID["a"].State = api.StateStopped
	m.mu.Unlock()
	if _, err := m.resolveVolumes(api.RunRequest{Volume: "notas"}); err != nil {
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
func TestElSnapshotRecuerdaSusVolumenes(t *testing.T) {
	mc := &api.Machine{Egress: "internet", Volumes: []api.VolumeAttachment{
		{Name: "notas", Mount: "/data"},
		{Name: "libs", Mount: "/libs", ReadOnly: true},
	}}
	snap := &api.Snapshot{Egress: mc.Egress, Volumes: mc.Volumes}

	got := snap.VolumeSet()
	if len(got) != 2 || got[0].Name != "notas" || got[1].Name != "libs" || !got[1].ReadOnly {
		t.Fatalf("el snapshot perdió sus volúmenes: %+v", got)
	}

	// Al despertar, una petición SIN volúmenes tiene que heredarlos: el gateway
	// despierta servicios por nombre y no sabe nada de volúmenes.
	req := api.RunRequest{From: "svc"}
	if len(req.VolumeSet()) == 0 {
		req.Volumes = snap.VolumeSet()
	}
	if len(req.VolumeSet()) != 2 {
		t.Errorf("no heredó los volúmenes del snapshot: %+v", req.VolumeSet())
	}

	// Una máquina sin volúmenes no debe parecer que tiene uno: eso obligaría a
	// pasar -volume para despertar servicios que no lo usan.
	if n := len((&api.Snapshot{}).VolumeSet()); n != 0 {
		t.Errorf("un snapshot sin volúmenes informó de %d", n)
	}
}

// Un snapshot CONGELADO ANTES de que hubiera varios volúmenes sigue arrancando.
//
// No es una cortesía: un snapshot ya congelado no se puede reescribir, así que
// si VolumeSet() no entendiera la forma antigua habría que reimportar todos los
// servicios existentes para estrenar esta versión — y quien no lo hiciera vería
// sus servicios despertar SIN volumen, escribiendo en un overlay que muere con
// la máquina y sin un solo error.
func TestUnSnapshotAntiguoSigueTeniendoSuVolumen(t *testing.T) {
	viejo := &api.Snapshot{Volume: "notas", VolumeMount: "/data", HasVolume: true}
	got := viejo.VolumeSet()
	if len(got) != 1 {
		t.Fatalf("perdió el volumen de un snapshot antiguo: %+v", got)
	}
	if got[0].Name != "notas" || got[0].Mount != "/data" || got[0].ReadOnly {
		t.Errorf("lo leyó mal: %+v", got[0])
	}

	// Y el identificador del disco tiene que ser el que quedó grabado entonces:
	// con el nuevo, PatchDrive apuntaría a un dispositivo que no existe y la
	// microVM restaurada miraría al fichero de nadie.
	if legacyVolumeDriveID == volumeDriveID(0) {
		t.Error("el identificador antiguo y el nuevo no pueden coincidir")
	}
	if legacyVolumeDriveID != "volume" {
		t.Errorf("el identificador antiguo cambió: %q", legacyVolumeDriveID)
	}
}

// Un escritor, o muchos lectores. Nunca las dos cosas.
//
// Es la regla que hace posible una biblioteca compartida: un volumen con
// paquetes que puebla una máquina y consumen muchas, sin duplicarlo en cada
// imagen. Y es la misma física de siempre — dos escritores corrompen un ext4,
// muchos lectores sobre bloques que no cambian no.
func TestUnEscritorOMuchosLectores(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("libs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	añadir := func(id, name string, ro bool) {
		m.mu.Lock()
		m.byID[id] = &api.Machine{ID: id, Name: name, State: api.StateRunning,
			Volumes: []api.VolumeAttachment{{Name: "libs", Mount: "/libs", ReadOnly: ro}}}
		m.mu.Unlock()
	}
	lector := api.RunRequest{Volume: "libs", VolumeReadOnly: true}
	escritor := api.RunRequest{Volume: "libs"}

	// Tres lectores concurrentes: los tres entran.
	for i, n := range []string{"a", "b", "c"} {
		if got, err := m.resolveVolumes(lector); err != nil || len(got) != 1 || !got[0].readOnly {
			t.Fatalf("lector %d rechazado: %v", i, err)
		}
		añadir(n, "svc-"+n, true)
	}

	// Un escritor con lectores dentro: no.
	_, err := m.resolveVolumes(escritor)
	if err == nil {
		t.Fatal("dejó escribir con lectores dentro")
	}
	if !strings.Contains(err.Error(), "svc-a") || !strings.Contains(err.Error(), "-volume libs:ro") {
		t.Errorf("el error debe decir quién lee y cómo entrar igualmente: %v", err)
	}

	// Sin lectores, el escritor entra.
	m.mu.Lock()
	m.byID = map[string]*api.Machine{}
	m.mu.Unlock()
	if got, err := m.resolveVolumes(escritor); err != nil || len(got) != 1 || got[0].readOnly {
		t.Fatalf("un volumen libre debería admitir escritor: %v", err)
	}
	añadir("w", "el-escritor", false)

	// Y con escritor dentro no entra NADIE, ni siquiera a leer: vería metadatos
	// cambiando bajo sus pies.
	for _, req := range []api.RunRequest{lector, escritor} {
		_, err := m.resolveVolumes(req)
		if err == nil {
			t.Errorf("dejó entrar con un escritor dentro (ro=%v)", req.VolumeReadOnly)
			continue
		}
		if !strings.Contains(err.Error(), "el-escritor") {
			t.Errorf("el error debe nombrar al escritor: %v", err)
		}
	}
}

// El modo viaja PEGADO al punto de montaje, para que el puente no pueda leer
// uno sin el otro: montar en escritura lo que se pidió de solo lectura
// corrompería lo que están leyendo las demás microVMs.
func TestElModoViajaConElPuntoDeMontaje(t *testing.T) {
	if got := volumeBootArg([]api.VolumeAttachment{{Mount: "/libs", ReadOnly: true}}); !strings.HasSuffix(got, "=/libs:ro") {
		t.Errorf("falta el sufijo :ro: %q", got)
	}
	if got := volumeBootArg([]api.VolumeAttachment{{Mount: "/data"}}); strings.Contains(got, ":ro") {
		t.Errorf("marcó de solo lectura lo que no lo es: %q", got)
	}
	// Y sin volúmenes no se añade nada: un kling.volume= vacío haría que el
	// puente intentara montar en "".
	if got := volumeBootArg(nil); got != "" {
		t.Errorf("sin volumen no debe añadir nada: %q", got)
	}
}

// Varios volúmenes en la misma microVM, que es de lo que va todo esto: el
// almacenamiento propio en escritura Y la biblioteca compartida en lectura.
func TestVariosVolumenesEnLaMismaMaquina(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"datos", "libs"} {
		if err := os.WriteFile(m.volumePath(n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	req := api.RunRequest{Volumes: []api.VolumeAttachment{
		{Name: "datos", Mount: "/data"},
		{Name: "libs", Mount: "/libs", ReadOnly: true},
	}}
	got, err := m.resolveVolumes(req)
	if err != nil {
		t.Fatalf("dos volúmenes deberían valer: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("esperaba 2, hay %d", len(got))
	}
	// El ORDEN es el contrato: es el orden de los discos y el de los puntos de
	// montaje en la línea del kernel. Si se altera, cada volumen se monta en el
	// sitio del otro y nadie da un error.
	if got[0].name != "datos" || got[1].name != "libs" {
		t.Errorf("se alteró el orden: %s, %s", got[0].name, got[1].name)
	}
	if got[0].readOnly || !got[1].readOnly {
		t.Errorf("se cruzaron los modos: %+v", got)
	}
	arg := volumeBootArg(attachments(got))
	if !strings.Contains(arg, "=/data,/libs:ro") {
		t.Errorf("la línea del kernel no lleva la lista en orden: %q", arg)
	}
}

// Los errores que evitan un desastre silencioso.
func TestVolumenesQueNoTienenSentido(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(m.volumePath(n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	casos := []struct {
		nombre string
		vols   []api.VolumeAttachment
		diga   string
	}{
		{
			// El mismo disco montado dos veces en la misma máquina es un doble
			// montaje tan malo como entre dos máquinas, y además se le escapa a
			// la comprobación de usuarios: la máquina aún no está registrada.
			"el mismo volumen dos veces",
			[]api.VolumeAttachment{{Name: "a", Mount: "/uno"}, {Name: "a", Mount: "/dos"}},
			"dos veces",
		},
		{
			// El segundo taparía al primero, que quedaría montado y fuera de
			// alcance: los ficheros existen y no se ven.
			"dos volúmenes en el mismo punto",
			[]api.VolumeAttachment{{Name: "a", Mount: "/x"}, {Name: "b", Mount: "/x"}},
			"taparía",
		},
		{
			// La lista viaja separada por comas en la línea del kernel.
			"una coma en el punto de montaje",
			[]api.VolumeAttachment{{Name: "a", Mount: "/con,coma"}},
			"sin espacios ni comas",
		},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			_, err := m.resolveVolumes(api.RunRequest{Volumes: c.vols})
			if err == nil {
				t.Fatal("debería rechazarlo")
			}
			if !strings.Contains(err.Error(), c.diga) {
				t.Errorf("el error no explica el problema: %v", err)
			}
		})
	}

	// Y el tope: cada volumen es un disco más, y los discos se nombran por letra.
	// Nombres y puntos DISTINTOS: si se repitieran, el input violaría tres
	// reglas a la vez y el test seguiría en verde aunque el tope desapareciera.
	muchos := make([]api.VolumeAttachment, api.MaxVolumes+1)
	for i := range muchos {
		muchos[i] = api.VolumeAttachment{
			Name:  fmt.Sprintf("v%d", i),
			Mount: fmt.Sprintf("/m%d", i),
		}
	}
	_, err := m.resolveVolumes(api.RunRequest{Volumes: muchos})
	if err == nil || !strings.Contains(err.Error(), "el máximo es") {
		t.Errorf("el tope no se aplicó: %v", err)
	}
}

// La ejecución de comandos solo se enciende cuando se pide, y solo la pide el
// daemon para las microVMs de un solo uso que pueblan un volumen.
//
// Si esto se colara en el arranque de un servicio, cualquiera que alcanzase al
// invitado podría ejecutar comandos dentro — y el gateway alcanza a todos los
// invitados por definición.
func TestLaEjecucionNoSeCuelaEnUnServicio(t *testing.T) {
	if got := execBootArg(false); got != "" {
		t.Errorf("añadió el parámetro sin pedirlo: %q", got)
	}
	got := execBootArg(true)
	if !strings.Contains(got, api.ExecBootParam+"=1") {
		t.Errorf("no lo añadió cuando se pidió: %q", got)
	}
	if !strings.HasPrefix(got, " ") {
		t.Errorf("se pegaría al argumento anterior: %q", got)
	}

	// Y la línea de arranque de un servicio normal no lo lleva por ningún lado.
	normal := bootArgs([]api.VolumeAttachment{{Mount: "/data"}}, false)
	if strings.Contains(normal, api.ExecBootParam) {
		t.Errorf("la línea de un servicio lleva el parámetro de ejecución: %s", normal)
	}
	conExec := bootArgs([]api.VolumeAttachment{{Mount: "/data"}}, true)
	if !strings.Contains(conExec, api.ExecBootParam+"=1") {
		t.Errorf("la microVM de instalación no lo lleva: %s", conExec)
	}
}
