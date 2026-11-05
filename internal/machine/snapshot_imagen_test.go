package machine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// conRestauracionBarata hace como si el backend restaurase barato (kling-hv,
// Firecracker), que es donde hay snapshots por imagen.
func conRestauracionBarata(t *testing.T) {
	t.Helper()
	viejo := restauraBarato
	restauraBarato = func() bool { return true }
	t.Cleanup(func() { restauraBarato = viejo })
	t.Setenv("KLING_IMAGE_SNAPSHOTS", "")
}

// managerConImagen es un manager de prueba con una imagen monolítica name y
// un kernel, lo que claveImagenDe necesita mirar.
func managerConImagen(t *testing.T, name string) *Manager {
	t.Helper()
	m := newTestManager(t)
	for _, d := range []string{"images", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(m.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(m.imagePath(name), []byte("rootfs de "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KernelPath(), []byte("vmlinux"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m
}

// rellenar pone un valor distinto de cero en cada campo exportado de v (un
// puntero a struct): lo que haría una petición que lo pide todo.
func rellenar(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("de-la-maquina")
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			s := reflect.MakeSlice(f.Type(), 1, 1)
			if s.Index(0).Kind() == reflect.String {
				s.Index(0).SetString("de-la-maquina=1")
			} else if s.Index(0).Kind() == reflect.Struct {
				rellenar(t, s.Index(0))
			}
			f.Set(s)
		case reflect.Map:
			mp := reflect.MakeMap(f.Type())
			mp.SetMapIndex(reflect.ValueOf("de-la-maquina"), reflect.ValueOf("1").Convert(f.Type().Elem()))
			f.Set(mp)
		case reflect.Pointer:
			p := reflect.New(f.Type().Elem())
			if p.Elem().Kind() == reflect.Struct {
				rellenar(t, p.Elem())
			}
			f.Set(p)
		case reflect.Struct:
			rellenar(t, f)
		default:
			t.Fatalf("campo %s de tipo %s: enséñale a rellenar a esta prueba", v.Type().Field(i).Name, f.Kind())
		}
	}
}

// La garantía central: la máquina de preparación —la que se congela y de la
// que salen todas las copias— no lleva NADA de la máquina que la provocó.
// Solo lo que fija la clave (y por tanto comparten todas sus copias), un
// nombre y una etiqueta propios y la red cerrada. El entorno, el comando, los
// secretos, los discos, las etiquetas y el nombre de quien la pidió llegan a
// cada copia DESPUÉS de restaurarla (POST /machine/start, runFromCon).
//
// Un campo nuevo de api.RunRequest que esta prueba no conozca hace que falle
// hasta que alguien decida si cambia el estado congelado (va a la clave) o es
// de la máquina (no va a la preparación).
func TestSnapshotImagenPrepNoLlevaNadaDeLaMaquina(t *testing.T) {
	var req api.RunRequest
	rellenar(t, reflect.ValueOf(&req).Elem())
	c := claveImagen{V: versionClaveImagen, Image: "img", VCPUs: 2, MemMiB: 256, MemMaxMiB: 512, DiskMiB: 1024, AllowExec: true}
	prep := peticionPrepImagen(prefijoPrepImagen+"abc", req, c)

	// De estado: los que fija la clave (o equivalentes a ella).
	deEstado := map[string]bool{"Image": true, "VCPUs": true, "MemMiB": true, "MemMaxMiB": true, "DiskMiB": true,
		"NetMiBps": true, "AllowExec": true}
	// Propios de la preparación.
	propios := map[string]bool{"Name": true, "Egress": true, "Labels": true, "CPUPct": true}
	// Todo campo de RunRequest tiene que estar decidido aquí o en deMaquina.
	deMaquina := map[string]bool{"From": true, "AllowDomains": true, "TTLSeconds": true, "CPUPctDefault": true,
		"WaitReady": true, "ReadyTimeoutSeconds": true, "Volumes": true, "Volume": true, "VolumeMount": true,
		"VolumeReadOnly": true, "Shares": true, "OnTTL": true, "Env": true, "ShareWait": true, "Publish": true,
		"Entrypoint": true, "Cmd": true, "VolumeWait": true, "Fresh": true, "HostPorts": true, "Nested": true}
	tipo := reflect.TypeOf(prep)
	v := reflect.ValueOf(prep)
	for i := 0; i < tipo.NumField(); i++ {
		nombre := tipo.Field(i).Name
		if !deEstado[nombre] && !propios[nombre] && !deMaquina[nombre] {
			t.Errorf("api.RunRequest.%s es nuevo: decide si cambia el estado congelado (clave de snapshot_imagen.go) o es de la máquina", nombre)
			continue
		}
		if deMaquina[nombre] && !v.Field(i).IsZero() {
			t.Errorf("la máquina de preparación lleva %s de la máquina que la pidió: %v", nombre, v.Field(i).Interface())
		}
	}
	if prep.Egress != "none" {
		t.Errorf("la preparación sale con egress %q: tiene que ir sin red", prep.Egress)
	}
	if !strings.HasPrefix(prep.Name, prefijoPrepImagen) || prep.Name == req.Name {
		t.Errorf("nombre de la preparación %q", prep.Name)
	}
	if len(prep.Labels) != 1 || prep.Labels[labelPrepImagen] != c.nombre() {
		t.Errorf("etiquetas de la preparación: %v", prep.Labels)
	}
	if prep.Image != c.Image || prep.VCPUs != c.VCPUs || prep.MemMiB != c.MemMiB || prep.MemMaxMiB != c.MemMaxMiB || prep.AllowExec != c.AllowExec {
		t.Errorf("la preparación no es la de su clave: %+v frente a %+v", prep, c)
	}
}

// Y la otra mitad: commit vuelve a mirarlo con el cerrojo tomado, por si a
// la máquina de preparación le llegó algo entre medias.
func TestSnapshotImagenComprobarPrep(t *testing.T) {
	limpia := &api.Machine{ID: "p", Name: prefijoPrepImagen + "x"}
	if err := comprobarPrepImagen(limpia); err != nil {
		t.Fatalf("una preparación limpia no pasa: %v", err)
	}
	for nombre, ensuciar := range map[string]func(*api.Machine){
		"secretos":     func(mc *api.Machine) { mc.HasSecrets = true },
		"entorno":      func(mc *api.Machine) { mc.EnvKeys = []string{"PASSWORD"} },
		"comando":      func(mc *api.Machine) { mc.Cmd = []string{"x"} },
		"entrypoint":   func(mc *api.Machine) { mc.Entrypoint = []string{"x"} },
		"volumen":      func(mc *api.Machine) { mc.Volumes = []api.VolumeAttachment{{Name: "v"}} },
		"carpeta":      func(mc *api.Machine) { mc.Shares = []api.ShareAttachment{{Mount: "/w"}} },
		"credenciales": func(mc *api.Machine) { mc.CredentialDomains = []string{"api.example.com"} },
	} {
		mc := *limpia
		ensuciar(&mc)
		if comprobarPrepImagen(&mc) == nil {
			t.Errorf("con %s se congelaría como snapshot por imagen", nombre)
		}
	}
}

// Lo que no es de estado no cambia la clave (todas esas máquinas comparten
// el snapshot, y por eso tiene que llegar después); lo que lo es, sí.
func TestSnapshotImagenClave(t *testing.T) {
	conRestauracionBarata(t)
	m := managerConImagen(t, "img")
	base := api.RunRequest{Image: "img", VCPUs: 1, MemMiB: 256}
	c0, err := m.claveImagenDe(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c0.nombre(), prefijoImagen) || !validName.MatchString(c0.nombre()) {
		t.Fatalf("nombre %q", c0.nombre())
	}
	igual := []func(*api.RunRequest){
		func(r *api.RunRequest) { r.Env = []string{"A=1"} },
		func(r *api.RunRequest) { r.Name = "otra" },
		func(r *api.RunRequest) { r.Labels = map[string]string{"owner": "t1"} },
		func(r *api.RunRequest) { r.Egress = "internet" },
		func(r *api.RunRequest) { r.Cmd = []string{"--x"} },
		func(r *api.RunRequest) { r.TTLSeconds = 60 },
		func(r *api.RunRequest) { r.CPUPct = 200 },
	}
	for i, f := range igual {
		r := base
		f(&r)
		if c, _ := m.claveImagenDe(r); c.nombre() != c0.nombre() {
			t.Errorf("caso %d: algo de la máquina cambia la clave", i)
		}
	}
	distinto := []func(*api.RunRequest){
		func(r *api.RunRequest) { r.VCPUs = 2 },
		func(r *api.RunRequest) { r.MemMiB = 512 },
		func(r *api.RunRequest) { r.MemMaxMiB = 1024 },
		func(r *api.RunRequest) { r.DiskMiB = 2048 },
		func(r *api.RunRequest) { r.AllowExec = true },
		func(r *api.RunRequest) { r.NetMiBps = 64 },
	}
	for i, f := range distinto {
		r := base
		f(&r)
		if c, _ := m.claveImagenDe(r); c.nombre() == c0.nombre() {
			t.Errorf("caso %d: un parámetro de estado no cambia la clave", i)
		}
	}
	// El disco por defecto y el explícito de 512 son el mismo.
	r := base
	r.DiskMiB = defaultOverlayMiB
	if c, _ := m.claveImagenDe(r); c.nombre() != c0.nombre() {
		t.Error("disk 0 y disk 512 dan claves distintas")
	}
	// Otra importación con el mismo nombre: otra clave.
	time.Sleep(10 * time.Millisecond)
	tmp := m.imagePath("img") + ".tmp"
	if err := os.WriteFile(tmp, []byte("otro rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, m.imagePath("img")); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.claveImagenDe(base); c.nombre() == c0.nombre() {
		t.Error("reimportar la imagen no cambia la clave")
	}
	// Y otro kernel.
	c1, _ := m.claveImagenDe(base)
	if err := os.WriteFile(m.KernelPath(), []byte("otro vmlinux más largo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.claveImagenDe(base); c.nombre() == c1.nombre() {
		t.Error("otro kernel no cambia la clave")
	}
}

func TestSnapshotImagenQuePeticiones(t *testing.T) {
	conRestauracionBarata(t)
	m := newTestManager(t)
	if motivo := m.motivoSinSnapshotImagen(api.RunRequest{Image: "img", Env: []string{"A=1"}, Cmd: []string{"x"}}); motivo != "" {
		t.Fatalf("una run con entorno y comando no vale: %s", motivo)
	}
	for nombre, r := range map[string]api.RunRequest{
		"fresh":    {Image: "img", Fresh: true},
		"volumen":  {Image: "img", Volumes: []api.VolumeAttachment{{Name: "v", Mount: "/data"}}},
		"carpetas": {Image: "img", Shares: []api.ShareSpec{{Source: "/x", Mount: "/w", Mode: "ro"}}},
		"espera":   {Image: "img", ShareWait: 1},
		"volwait":  {Image: "img", VolumeWait: true},
	} {
		if m.motivoSinSnapshotImagen(r) == "" {
			t.Errorf("%s: saldría de un snapshot por imagen", nombre)
		}
	}
	t.Setenv("KLING_IMAGE_SNAPSHOTS", "0")
	if m.motivoSinSnapshotImagen(api.RunRequest{Image: "img"}) == "" {
		t.Error("KLING_IMAGE_SNAPSHOTS=0 no los apaga")
	}
	t.Setenv("KLING_IMAGE_SNAPSHOTS", "")
	restauraBarato = func() bool { return false } // kling-vz: restaurar carga la RAM entera
	if m.motivoSinSnapshotImagen(api.RunRequest{Image: "img"}) == "" {
		t.Error("sin restauración barata siguen activos")
	}
}

// El entorno (y el comando) de cada copia viajan en POST /machine/start, a
// la copia y solo a ella, una vez restaurada.
func TestSnapshotImagenEntregaLoDeLaMaquina(t *testing.T) {
	var recibido api.GuestStart
	var veces int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestStartPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		veces++
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &recibido); err != nil {
			t.Errorf("cuerpo: %v", err)
		}
		if veces > 1 {
			http.Error(w, "this machine was already started", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	m := newTestManager(t)
	mc := m.addForTest("copia")
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": u.Host}
	m.mu.Unlock()

	ai := &arranqueImagen{env: map[string]string{"PASSWORD": "s3cr3t"}, comando: &api.MachineCmd{Cmd: []string{"--port", "9"}}}
	if err := m.entregarMaquina(context.Background(), "copia", ai); err != nil {
		t.Fatal(err)
	}
	if recibido.Env["PASSWORD"] != "s3cr3t" || recibido.Cmd == nil || recibido.Cmd.Cmd[1] != "9" {
		t.Fatalf("el agente recibió %+v", recibido)
	}
	// Un agente que ya arrancó (en el volcado había una máquina arrancada):
	// la copia no vale.
	if err := m.entregarMaquina(context.Background(), "copia", ai); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("un 409 no es un fallo: %v", err)
	}
}

// escribirSnapshotImagen deja en disco un snapshot por imagen de c como lo
// deja commit: marca antes que meta.json.
func escribirSnapshotImagen(t *testing.T, m *Manager, c claveImagen, mib int, uso time.Time) string {
	t.Helper()
	dir := m.snapDir(c.nombre())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, marcaImagen), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, mib<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(api.Snapshot{Name: c.nombre(), Image: c.Image, CreatedAt: time.Now(), VCPUs: c.VCPUs, MemMiB: c.MemMiB})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, marcaImagen), uso, uso); err != nil {
		t.Fatal(err)
	}
	return c.nombre()
}

// No son dorados: no salen en `kling snapshots` (ni retienen la imagen),
// salen en su lista, una run -from no los usa, y el tope de disco los poda
// del menos usado al más, nunca uno con copias vivas.
func TestSnapshotImagenGestion(t *testing.T) {
	conRestauracionBarata(t)
	m := managerConImagen(t, "img")
	var claves []claveImagen
	for _, mem := range []int{256, 512, 1024} {
		c, err := m.claveImagenDe(api.RunRequest{Image: "img", VCPUs: 1, MemMiB: mem})
		if err != nil {
			t.Fatal(err)
		}
		claves = append(claves, c)
	}
	ahora := time.Now()
	viejo := escribirSnapshotImagen(t, m, claves[0], 1, ahora.Add(-3*time.Hour))
	medio := escribirSnapshotImagen(t, m, claves[1], 1, ahora.Add(-2*time.Hour))
	nuevo := escribirSnapshotImagen(t, m, claves[2], 1, ahora.Add(-time.Hour))
	if !m.snapshotImagenListo(claves[0]) {
		t.Fatal("no lo ve listo")
	}
	for _, s := range m.Snapshots() {
		if strings.HasPrefix(s.Name, prefijoImagen) {
			t.Fatalf("%s sale en kling snapshots", s.Name)
		}
	}
	if l := m.SnapshotsImagen(); len(l) != 3 || l[0].Name != nuevo || l[0].Stale != "" {
		t.Fatalf("lista: %+v", l)
	}
	if _, err := m.run(context.Background(), api.RunRequest{From: viejo}); err == nil || !strings.Contains(err.Error(), "image snapshot") {
		t.Fatalf("run -from de un snapshot por imagen: %v", err)
	}

	// El viejo tiene una copia viva: no se poda aunque sea el menos usado.
	mc := m.addForTest("copia")
	m.mu.Lock()
	mc.ImageSnapshot = viejo
	m.mu.Unlock()
	tamUno := m.SnapshotsImagen()[0].DiskBytes
	m.podarSnapshotsImagen(2*tamUno + tamUno/2) // caben dos
	quedan := map[string]bool{}
	for _, s := range m.SnapshotsImagen() {
		quedan[s.Name] = true
	}
	if !quedan[viejo] || quedan[medio] || !quedan[nuevo] {
		t.Fatalf("tras podar quedan %v (viejo con copia, medio el menos usado sin ella)", quedan)
	}

	// Reimportar la imagen los deja obsoletos; sin copias, el barrido los
	// borra.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(m.imagePath("img")+".n", []byte("otra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(m.imagePath("img")+".n", m.imagePath("img")); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.SnapshotsImagen() {
		if s.Stale == "" {
			t.Errorf("%s sigue valiendo tras reimportar la imagen", s.Name)
		}
	}
	m.podarSnapshotsImagen(1 << 40)
	if l := m.SnapshotsImagen(); len(l) != 1 || l[0].Name != viejo {
		t.Fatalf("tras el barrido: %+v (solo debía quedar el que tiene copia)", l)
	}
	// image rm se niega mientras la copia viva lo use; sin ella, se lo lleva.
	if err := m.borrarSnapshotsImagenDe("img"); err == nil {
		t.Fatal("borró un snapshot por imagen con una copia viva")
	}
	m.mu.Lock()
	mc.State = api.StateStopped
	m.mu.Unlock()
	if err := m.borrarSnapshotsImagenDe("img"); err != nil {
		t.Fatal(err)
	}
	if l := m.SnapshotsImagen(); len(l) != 0 {
		t.Fatalf("image rm deja %+v", l)
	}
}

// Una máquina de un snapshot por imagen es, para el resto del daemon, una
// arrancada en frío (From vacío) que mapea la memoria de su snapshot.
func TestSnapshotImagenOrigen(t *testing.T) {
	mc := &api.Machine{ImageSnapshot: "kling-img-x"}
	if snapshotDeOrigen(mc) != "kling-img-x" || !mapeaSuDorado(mc) {
		t.Fatal("no cuenta como instancia de su snapshot")
	}
	mc.BootMS = 3 // kling start: arrancó en frío sobre su disco
	if mapeaSuDorado(mc) {
		t.Fatal("tras un start en frío sigue contando como que mapea el snapshot")
	}
	if snapshotDeOrigen(&api.Machine{From: "d", ImageSnapshot: "x"}) != "d" {
		t.Fatal("From manda")
	}
	var inc *errImagenIncompatible
	if !errors.As(error(&errImagenIncompatible{motivo: "x"}), &inc) {
		t.Fatal("errImagenIncompatible")
	}
}

// Una máquina de preparación que dejó un daemon muerto a mitad se barre; una
// en marcha o una del usuario con un nombre parecido, no.
func TestSnapshotImagenBarrePreparacionesHuerfanas(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	huerfana := m.addForTest("h")
	enMarcha := m.addForTest("e")
	delUsuario := m.addForTest("u")
	m.mu.Lock()
	huerfana.Name, huerfana.Labels = prefijoPrepImagen+"viejo", map[string]string{labelPrepImagen: "kling-img-x"}
	huerfana.State = api.StateStopped
	enMarcha.Name, enMarcha.Labels = prefijoPrepImagen+"ahora", map[string]string{labelPrepImagen: "kling-img-y"}
	enMarcha.State = api.StateStopped
	delUsuario.Name = prefijoPrepImagen + "mia"
	delUsuario.State = api.StateStopped
	m.mu.Unlock()
	m.img.mu.Lock()
	m.img.iniciar()
	m.img.prep[enMarcha.Name] = true
	m.img.mu.Unlock()
	m.barrerPrepImagen()
	// Borrar deja una tumba que se limpia en segundo plano (borrado.go).
	vive := func(id string) bool { mc, ok := m.Get(id); return ok && !esTumba(mc) }
	if vive("h") {
		t.Error("la huérfana sigue")
	}
	if !vive("e") {
		t.Error("borró una preparación en marcha")
	}
	if !vive("u") {
		t.Error("borró una máquina del usuario sin la etiqueta")
	}
}
