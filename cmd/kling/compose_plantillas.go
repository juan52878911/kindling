package main

// Plantillas calientes de `kling compose up`.
//
// Lo que cuesta un `up` en kindling es arrancar en frío: postgres hace initdb
// cada vez (5 s frente a 1,5 s en Docker) y cada máquina paga su arranque. Así
// que la primera vez cada servicio se arranca SUELTO (una máquina de
// preparación), se espera a que esté listo —la sonda de la imagen y, si el
// compose la declara, su `healthcheck` ejecutado dentro— y se guarda como
// plantilla; desde entonces el nodo del grafo sale de ella (`from:`) y se
// restaura en milisegundos, sin reimportar nada.
//
// La plantilla se llama <proyecto>-<nodo>-<clave>: la clave es un hash de la
// receta (la orden de import, el entorno, el comando, las vCPU y la memoria,
// las carpetas y el healthcheck, y la versión del daemon). Cambiar cualquiera
// de ellas es otra clave: se prepara otra plantilla y se borra la vieja.
// Lo que no se ve es una etiqueta que se mueve en el registro (como Docker,
// que tampoco vuelve a bajar la imagen): `compose up -fresh` las rehace.
//
// El entorno no se hornea en la imagen: va en la máquina de preparación
// (RunRequest.Env) y queda en la memoria de la plantilla. Una sola imagen por
// imagen y comando; cambiar una variable no reimporta.
//
// Un servicio con carpetas vivas (bind mounts) se prepara con su agente
// esperando a esas carpetas (RunRequest.ShareWait) y sin dárselas: la
// plantilla es la máquina arrancada con el servicio aún sin lanzar. Al
// restaurarla, el daemon le conecta las carpetas y el servicio arranca con el
// código de ahora, no con el de cuando se guardó.
//
// Un servicio con volúmenes con nombre se prepara igual pero con su agente
// esperando a que el host le monte los volúmenes (RunRequest.VolumeWait) y el
// servicio sin lanzar: congelar un postgres con su directorio de datos montado
// dejaría en la memoria de la plantilla la caché de un disco que después
// cambia. Cada copia los monta al restaurarse y entonces arranca el servicio,
// sobre los datos de ahora. Con un daemon sin api.CapabilityVolumeWait, esos
// servicios arrancan en frío, como antes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas de las máquinas de preparación, que heredan sus plantillas: de
// qué proyecto y nodo son, para borrar las que ya no valen.
const (
	labelComposeProject = "kling.compose"
	labelComposeNode    = "kling.compose.node"
)

// plantillaCompose es lo que compose up decide de un servicio.
type plantillaCompose struct {
	s      *servicioCompose
	nombre string // la plantilla; vacío = arranque en frío (volúmenes)
	existe bool
	vivas  int // carpetas vivas que espera
	// volEspera: se prepara con sus volúmenes sin montar (VolumeWait).
	volEspera bool
	// cmdMaquina: command: y entrypoint: van en la máquina de preparación
	// (RunRequest.Cmd/Entrypoint), no horneados en la imagen.
	cmdMaquina bool
}

// daemonCompose es lo que la receta de una plantilla toma del daemon: su
// versión, su VMM (una plantilla de kling-vz no se restaura en kling-hv) y si
// entiende el comando por máquina.
type daemonCompose struct {
	version, backend string
	cmd, volWait     bool
}

// daemonDeInfo saca daemonCompose de GET /info.
func daemonDeInfo(info *api.Info) daemonCompose {
	return daemonCompose{version: info.Version, backend: info.Backend,
		cmd:     slices.Contains(info.Capabilities, api.CapabilityMachineCmd),
		volWait: slices.Contains(info.Capabilities, api.CapabilityVolumeWait)}
}

// usaPlantilla dice si el servicio sale de una plantilla: todos, salvo los que
// llevan volúmenes con nombre si el daemon no sabe esperarlos.
func usaPlantilla(s *servicioCompose, d daemonCompose) bool { return len(s.vols) == 0 || d.volWait }

// clavePlantilla es el hash de todo lo que decide la plantilla de s. El
// comando está siempre: aun yendo en la máquina, solo se aplica en frío (la
// plantilla guarda la memoria con el servicio lanzado, o a punto de lanzarse
// con lo que su agente leyó al arrancar), así que otro comando es otra
// plantilla; lo que se ahorra es reimportar la imagen.
func clavePlantilla(s *servicioCompose, proyecto string, d daemonCompose) string {
	receta := struct {
		Import  string
		Env     [][2]string
		Cmd     []string
		Entry   []string
		VCPUs   int
		MemMiB  int
		Shares  []api.ShareSpec
		Vols    []api.VolumeAttachment
		Salud   []string
		Daemon  string
		Backend string
		Version int
	}{ordenImportCon(s, proyecto, false, !d.cmd, false), s.env, s.cmd, s.entry, s.vcpus, s.memMiB, s.shares, s.vols, s.salud,
		d.version, d.backend, 2}
	b, _ := json.Marshal(receta)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:4])
}

// nombrePlantilla es <proyecto>-<nodo>-<clave>, acortado a lo que admite un
// nombre de plantilla (64).
func nombrePlantilla(proyecto, nodo, clave string) string {
	base := proyecto + "-" + nodo
	if len(base) > 55 {
		base = strings.TrimRight(base[:55], "-_.")
	}
	return base + "-" + clave
}

// planPlantillas decide, para cada servicio, de qué plantilla sale y si ya
// existe. fresh hace como que ninguna existe.
func planPlantillas(t *traduccionCompose, snaps []*api.Snapshot, d daemonCompose, fresh bool) []*plantillaCompose {
	hay := map[string]bool{}
	for _, sn := range snaps {
		hay[sn.Name] = true
	}
	var out []*plantillaCompose
	for _, s := range t.vivos {
		p := &plantillaCompose{s: s}
		if usaPlantilla(s, d) {
			p.nombre = nombrePlantilla(t.Proyect, s.nodo, clavePlantilla(s, t.Proyect, d))
			p.existe = hay[p.nombre] && !fresh
			p.vivas = len(s.shares)
			p.cmdMaquina = d.cmd
			p.volEspera = len(s.vols) > 0
		}
		out = append(out, p)
	}
	return out
}

// viejasDe son las plantillas de compose del nodo que no son la actual.
func viejasDe(snaps []*api.Snapshot, proyecto, nodo, actual string) []string {
	var out []string
	for _, sn := range snaps {
		if sn.Labels[labelComposeProject] == proyecto && sn.Labels[labelComposeNode] == nodo && sn.Name != actual {
			out = append(out, sn.Name)
		}
	}
	return out
}

// plantillaOps es lo que la preparación necesita del daemon; *api.Client lo
// cumple y el test lo sustituye.
type plantillaOps interface {
	Run(ctx context.Context, r api.RunRequest) (*api.Machine, error)
	Exec(ctx context.Context, ref string, r api.ExecRequest, onOutput func(stream string, data []byte) error) (*api.ExecResult, error)
	CommitWith(ctx context.Context, ref string, req api.CommitRequest) (*api.Snapshot, error)
	Remove(ctx context.Context, ref string) error
}

// saludArgv es el test de un healthcheck como orden: ["CMD", a...] es a...;
// ["CMD-SHELL", x] y una cadena suelta, sh -c x.
func saludArgv(test []string) []string {
	switch {
	case len(test) == 0:
		return nil
	case test[0] == "CMD" && len(test) > 1:
		return test[1:]
	case test[0] == "CMD-SHELL" && len(test) > 1:
		return []string{"sh", "-c", strings.Join(test[1:], " ")}
	case test[0] == "CMD" || test[0] == "CMD-SHELL" || test[0] == "NONE":
		return nil
	}
	return []string{"sh", "-c", strings.Join(test, " ")}
}

// Plazos de la preparación.
var (
	plantillaListo = 2 * time.Minute
	// Entre dos intentos de esperarOrden se espera plantillaPasoMin y se
	// dobla hasta plantillaPaso: un healthcheck que pasa enseguida no paga
	// 200 ms por intento.
	plantillaPasoMin = 25 * time.Millisecond
	plantillaPaso    = 200 * time.Millisecond
	plantillaAgente  = 30 * time.Second
)

// prepararPlantilla arranca la máquina de preparación de p, espera a que esté
// lista (o, con carpetas, a que su agente conteste), corre su healthcheck y
// la guarda. La máquina se retira siempre.
func prepararPlantilla(ctx context.Context, ops plantillaOps, proyecto string, p *plantillaCompose, logf func(string, ...any)) error {
	s := p.s
	prep := p.nombre + "-prep"
	var env []string
	for _, kv := range s.env {
		env = append(env, kv[0]+"="+kv[1])
	}
	vcpus := max(s.vcpus, 1)
	// Esperando algo (carpetas o volúmenes), el servicio no arranca en la
	// máquina de preparación: se guarda en cuanto el agente contesta.
	espera := p.vivas > 0 || p.volEspera
	req := api.RunRequest{
		Name: prep, Image: proyecto + "-" + s.nodo, VCPUs: vcpus, MemMiB: s.memMiB,
		// CPU entera: la plantilla la conserva y sus copias arrancan sin el
		// techo de media vCPU (en desarrollo no hay vecinos que proteger).
		CPUPct: vcpus * 100, AllowExec: true, Env: env, ShareWait: p.vivas,
		Cmd: comandoMaquina(p, s.cmd), Entrypoint: comandoMaquina(p, s.entry),
		WaitReady: !espera, ReadyTimeoutSeconds: int(plantillaListo.Seconds()),
		Labels: map[string]string{labelComposeProject: proyecto, labelComposeNode: s.nodo},
		// Red de desarrollo: las carpetas viajan por ella y las copias
		// heredan este caudal (el de 16 MiB/s las ahogaba).
		NetMiBps: netMiBpsCarpetas,
	}
	if p.volEspera {
		// Los mismos volúmenes que el nodo: el disco del dorado tiene que
		// ser del tamaño del de sus copias. Puestos y sin montar.
		req.Volumes, req.VolumeWait = s.vols, true
	}
	if req.MemMiB == 0 {
		req.MemMiB = 512
	}
	// Una -prep de un up anterior al que Ctrl-C pilló dentro de su Run (el
	// daemon la creó, el CLI no llegó a borrarla) bloquearía el nombre: fuera
	// primero (un 404 es lo normal). Y si este Run falla o se corta, también:
	// el daemon pudo crearla igual.
	_ = ops.Remove(context.WithoutCancel(ctx), prep)
	mc, err := ops.Run(ctx, req)
	defer func() { _ = ops.Remove(context.WithoutCancel(ctx), prep) }()
	if err != nil {
		return fmt.Errorf("starting %s: %w", prep, err)
	}
	if !espera && (mc.Ready == api.ReadyWaiting || mc.Ready == api.ReadyFailed) {
		return fmt.Errorf("%s is running but not ready (%s): see kling logs %s", prep, mc.Ready, prep)
	}
	// Con carpetas o volúmenes, el servicio no arranca hasta tenerlos: basta
	// con que el agente conteste. Sin ellos, el healthcheck del compose, dentro.
	argv := saludArgv(s.salud)
	// El plazo del healthcheck (start_period + retries × interval), y nunca
	// menos que el de siempre: la primera vez hay initdb y se espera a ella.
	// Ni más que lo que esperaría el daemon (api.GraphHealthMaxWait).
	intervalo, plazo, reintentos, arranque, _ := s.saludT.Tiempos()
	limite := min(arranque+time.Duration(reintentos)*intervalo, api.GraphHealthMaxWait)
	limite, que := max(plantillaListo, limite), "healthcheck"
	if espera {
		argv, limite, que, plazo = []string{"true"}, plantillaAgente, "guest agent", 30*time.Second
	}
	if len(argv) > 0 {
		if err := esperarOrden(ctx, ops, prep, argv, limite, plazo); err != nil {
			return fmt.Errorf("%s: %s: %w", s.nombre, que, err)
		}
	}
	logf("saving template %s", p.nombre)
	_, err = ops.CommitWith(ctx, prep, api.CommitRequest{Name: p.nombre, Replace: true,
		SkipReady: espera, ReadyTimeoutSeconds: int(plantillaListo.Seconds())})
	return err
}

// comandoMaquina es l si va en la máquina, o nil.
func comandoMaquina(p *plantillaCompose, l []string) []string {
	if !p.cmdMaquina || len(l) == 0 {
		return nil
	}
	return l
}

// esperarOrden repite argv dentro de la máquina hasta que acaba con 0, como
// mucho d.
func esperarOrden(ctx context.Context, ops plantillaOps, ref string, argv []string, d, plazo time.Duration) error {
	limite := time.Now().Add(d)
	paso := min(plantillaPasoMin, plantillaPaso)
	var ultimo error
	for {
		// Con el usuario del servicio (el user: del compose), como Docker.
		res, err := ops.Exec(ctx, ref, api.ExecRequest{Cmd: argv, TimeoutSeconds: max(int(plazo.Seconds()), 1), User: api.ExecUserService},
			func(string, []byte) error { return nil })
		switch {
		case err == nil && res.ExitCode == 0 && !res.TimedOut:
			return nil
		case err != nil:
			ultimo = err
		default:
			ultimo = fmt.Errorf("%q exited with %d", strings.Join(argv, " "), res.ExitCode)
		}
		if time.Now().After(limite) {
			return fmt.Errorf("not passing after %s: %w", d, ultimo)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(paso):
		}
		paso = min(2*paso, plantillaPaso)
	}
}

// paraleloPlantillas: cuántas máquinas de preparación a la vez. Cada una
// arranca con su memoria y a CPU entera; todas a la vez, con 20 o 30
// servicios, se comían la memoria del Mac (jetsam).
const paraleloPlantillas = 4

// prepararPlantillas prepara las que faltan, paraleloPlantillas a la vez; el
// primer fallo corta las demás (el up ya no va a seguir).
func prepararPlantillas(ctx context.Context, ops plantillaOps, proyecto string, ps []*plantillaCompose, log io.Writer) error {
	var mu sync.Mutex
	logf := func(f string, a ...any) {
		mu.Lock()
		fmt.Fprintf(log, f+"\n", a...)
		mu.Unlock()
	}
	var faltan []*plantillaCompose
	for _, p := range ps {
		if p.nombre != "" && !p.existe {
			faltan = append(faltan, p)
		}
	}
	return enParaleloHastaFallo(ctx, len(faltan), paraleloPlantillas, func(ctx context.Context, i int) error {
		return prepararPlantilla(ctx, ops, proyecto, faltan[i], logf)
	})
}

// preparacionCompose es lo que compose up prepara para el grafo de t, en dos
// tiempos: antes (importarAntes) lo que no toca el proyecto que pueda estar
// levantado, y terminar, ya sin él.
type preparacionCompose struct {
	t          *traduccionCompose
	c          *api.Client
	ps         []*plantillaCompose
	snaps      []*api.Snapshot
	vols       []string
	imports    []importCompose
	pendientes []importCompose
	run        func(context.Context, string) (string, error)
}

// planCompose decide qué falta: los volúmenes, las imágenes que falten o
// hayan cambiado y las plantillas que falten.
func planCompose(ctx context.Context, c *api.Client, host string, t *traduccionCompose, fresh bool) (*preparacionCompose, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	p := &preparacionCompose{t: t, c: c, snaps: snaps, vols: volumenesDe(t),
		run: func(ctx context.Context, l string) (string, error) { return lineaKling(ctx, host, l) }}
	for i, v := range p.vols {
		p.vols[i] = "kling volume create " + v
	}
	t.saludTCP = !slices.Contains(info.Capabilities, api.CapabilityGraphHealth)
	if slices.Contains(info.Capabilities, api.CapabilityMachineEnv) {
		p.ps = planPlantillas(t, snaps, daemonDeInfo(info), fresh)
	} else {
		// Sin entorno en la máquina, el de siempre: horneado y en frío.
		fmt.Fprintf(os.Stderr, "warning: the daemon (%s) does not take a machine environment: no warm templates; update it\n", info.Version)
		for _, s := range t.vivos {
			p.ps = append(p.ps, &plantillaCompose{s: s})
		}
	}
	for _, pl := range p.ps {
		if pl.existe {
			continue
		}
		frio := pl.nombre == ""
		hornear := frio || !pl.cmdMaquina
		nodo := pl.s.nodo
		p.imports = append(p.imports, importCompose{
			linea:      ordenImportCon(pl.s, t.Proyect, frio, hornear, false),
			conReplace: ordenImportCon(pl.s, t.Proyect, frio, hornear, true),
			// Con -fresh, -replace siempre: la receta igual no ve un agente
			// nuevo (el de un kling actualizado), y -fresh es para eso.
			yaReplace: fresh,
			// La imagen cambia: las plantillas que salieron de ella, fuera.
			antesDeReplace: func() { p.borrar(ctx, viejasDe(snaps, t.Proyect, nodo, "")) },
		})
	}
	return p, nil
}

func (p *preparacionCompose) borrar(ctx context.Context, nombres []string) {
	for _, n := range nombres {
		_ = p.c.RemoveSnapshot(context.WithoutCancel(ctx), n)
	}
}

// importarAntes crea los volúmenes y corre los imports que no reemplazan
// nada (importarSinReemplazar): un fallo aquí deja el proyecto levantado
// como estaba.
func (p *preparacionCompose) importarAntes(ctx context.Context) error {
	if err := crearVolumenes(ctx, p.run, p.vols); err != nil {
		return err
	}
	var err error
	p.pendientes, err = importarSinReemplazar(ctx, p.run, p.imports)
	return err
}

// terminar, ya sin el proyecto anterior: los imports con -replace, las
// plantillas que faltan, y t.Grafo reescrito con los nodos que salen de
// plantilla. Las plantillas que ya no valen se borran.
func (p *preparacionCompose) terminar(ctx context.Context) error {
	if err := importarReemplazando(ctx, p.run, p.pendientes); err != nil {
		return err
	}
	if err := prepararPlantillas(ctx, p.c, p.t.Proyect, p.ps, os.Stderr); err != nil {
		return err
	}
	t := p.t
	t.desde = map[string]string{}
	for _, pl := range p.ps {
		if pl.nombre != "" {
			t.desde[pl.s.nodo] = pl.nombre
			p.borrar(ctx, viejasDe(p.snaps, t.Proyect, pl.s.nodo, pl.nombre))
		}
	}
	t.Grafo = grafoDeCompose(t, t.Proyect, t.vivos, t.por, func(string, ...any) {})
	return nil
}
