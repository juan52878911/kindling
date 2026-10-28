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
// Un servicio con volúmenes con nombre arranca en frío como antes: congelar
// un postgres con su directorio de datos en un volumen dejaría en la memoria
// de la plantilla la caché de un disco que después cambia.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// cmdMaquina: command: y entrypoint: van en la máquina de preparación
	// (RunRequest.Cmd/Entrypoint), no horneados en la imagen.
	cmdMaquina bool
}

// daemonCompose es lo que la receta de una plantilla toma del daemon: su
// versión, su VMM (una plantilla de kling-vz no se restaura en kling-hv) y si
// entiende el comando por máquina.
type daemonCompose struct {
	version, backend string
	cmd              bool
}

// daemonDeInfo saca daemonCompose de GET /info.
func daemonDeInfo(info *api.Info) daemonCompose {
	return daemonCompose{version: info.Version, backend: info.Backend,
		cmd: slices.Contains(info.Capabilities, api.CapabilityMachineCmd)}
}

// usaPlantilla dice si el servicio sale de una plantilla: todos menos los que
// llevan volúmenes con nombre.
func usaPlantilla(s *servicioCompose) bool { return len(s.vols) == 0 }

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
		Salud   []string
		Daemon  string
		Backend string
		Version int
	}{ordenImportCon(s, proyecto, false, !d.cmd, false), s.env, s.cmd, s.entry, s.vcpus, s.memMiB, s.shares, s.salud,
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
		if usaPlantilla(s) {
			p.nombre = nombrePlantilla(t.Proyect, s.nodo, clavePlantilla(s, t.Proyect, d))
			p.existe = hay[p.nombre] && !fresh
			p.vivas = len(s.shares)
			p.cmdMaquina = d.cmd
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
	plantillaListo  = 2 * time.Minute
	plantillaPaso   = 200 * time.Millisecond
	plantillaAgente = 30 * time.Second
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
	req := api.RunRequest{
		Name: prep, Image: proyecto + "-" + s.nodo, VCPUs: vcpus, MemMiB: s.memMiB,
		// CPU entera: la plantilla la conserva y sus copias arrancan sin el
		// techo de media vCPU (en desarrollo no hay vecinos que proteger).
		CPUPct: vcpus * 100, AllowExec: true, Env: env, ShareWait: p.vivas,
		Cmd: comandoMaquina(p, s.cmd), Entrypoint: comandoMaquina(p, s.entry),
		WaitReady: p.vivas == 0, ReadyTimeoutSeconds: int(plantillaListo.Seconds()),
		Labels: map[string]string{labelComposeProject: proyecto, labelComposeNode: s.nodo},
	}
	if req.MemMiB == 0 {
		req.MemMiB = 512
	}
	mc, err := ops.Run(ctx, req)
	if err != nil {
		return fmt.Errorf("starting %s: %w", prep, err)
	}
	defer func() { _ = ops.Remove(context.WithoutCancel(ctx), prep) }()
	if p.vivas == 0 && (mc.Ready == api.ReadyWaiting || mc.Ready == api.ReadyFailed) {
		return fmt.Errorf("%s is running but not ready (%s): see kling logs %s", prep, mc.Ready, prep)
	}
	// Con carpetas, el servicio no arranca hasta tenerlas: basta con que el
	// agente conteste. Sin ellas, el healthcheck del compose, dentro.
	argv := saludArgv(s.salud)
	// El plazo del healthcheck (start_period + retries × interval), y nunca
	// menos que el de siempre: la primera vez hay initdb y se espera a ella.
	intervalo, plazo, reintentos, arranque, _ := s.saludT.Tiempos()
	espera, que := max(plantillaListo, arranque+time.Duration(reintentos)*intervalo), "healthcheck"
	if p.vivas > 0 {
		argv, espera, que, plazo = []string{"true"}, plantillaAgente, "guest agent", 30*time.Second
	}
	if len(argv) > 0 {
		if err := esperarOrden(ctx, ops, prep, argv, espera, plazo); err != nil {
			return fmt.Errorf("%s: %s: %w", s.nombre, que, err)
		}
	}
	logf("saving template %s", p.nombre)
	_, err = ops.CommitWith(ctx, prep, api.CommitRequest{Name: p.nombre, Replace: true,
		SkipReady: p.vivas > 0, ReadyTimeoutSeconds: int(plantillaListo.Seconds())})
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
	var ultimo error
	for {
		res, err := ops.Exec(ctx, ref, api.ExecRequest{Cmd: argv, TimeoutSeconds: max(int(plazo.Seconds()), 1)},
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
		case <-time.After(plantillaPaso):
		}
	}
}

// prepararPlantillas prepara en paralelo las que faltan.
func prepararPlantillas(ctx context.Context, ops plantillaOps, proyecto string, ps []*plantillaCompose, log io.Writer) error {
	var mu sync.Mutex
	logf := func(f string, a ...any) {
		mu.Lock()
		fmt.Fprintf(log, f+"\n", a...)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ps))
	for i, p := range ps {
		if p.nombre == "" || p.existe {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = prepararPlantilla(ctx, ops, proyecto, p, logf)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// prepararCompose deja listo lo que el grafo de t necesita: los volúmenes,
// las imágenes que falten o hayan cambiado y las plantillas que falten, y
// reescribe t.Grafo con los nodos que salen de plantilla. Las plantillas que
// ya no valen se borran.
func prepararCompose(ctx context.Context, c *api.Client, host string, t *traduccionCompose, fresh bool) error {
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	var ps []*plantillaCompose
	t.saludTCP = !slices.Contains(info.Capabilities, api.CapabilityGraphHealth)
	if slices.Contains(info.Capabilities, api.CapabilityMachineEnv) {
		ps = planPlantillas(t, snaps, daemonDeInfo(info), fresh)
	} else {
		// Sin entorno en la máquina, el de siempre: horneado y en frío.
		fmt.Fprintf(os.Stderr, "warning: the daemon (%s) does not take a machine environment: no warm templates; update it\n", info.Version)
		for _, s := range t.vivos {
			ps = append(ps, &plantillaCompose{s: s})
		}
	}
	borrar := func(nombres []string) {
		for _, n := range nombres {
			_ = c.RemoveSnapshot(context.WithoutCancel(ctx), n)
		}
	}
	var vols []string
	for _, l := range t.Antes {
		if strings.HasPrefix(l, "kling volume create ") {
			vols = append(vols, l)
		}
	}
	var imports []importCompose
	for _, p := range ps {
		if p.existe {
			continue
		}
		frio := p.nombre == ""
		hornear := frio || !p.cmdMaquina
		nodo := p.s.nodo
		imports = append(imports, importCompose{
			linea:      ordenImportCon(p.s, t.Proyect, frio, hornear, false),
			conReplace: ordenImportCon(p.s, t.Proyect, frio, hornear, true),
			// La imagen cambia: las plantillas que salieron de ella, fuera.
			antesDeReplace: func() { borrar(viejasDe(snaps, t.Proyect, nodo, "")) },
		})
	}
	if err := runSetup(ctx, host, vols, imports); err != nil {
		return err
	}
	if err := prepararPlantillas(ctx, c, t.Proyect, ps, os.Stderr); err != nil {
		return err
	}
	t.desde = map[string]string{}
	for _, p := range ps {
		if p.nombre != "" {
			t.desde[p.s.nodo] = p.nombre
			borrar(viejasDe(snaps, t.Proyect, p.s.nodo, p.nombre))
		}
	}
	t.Grafo = grafoDeCompose(t, t.Proyect, t.vivos, t.por, func(string, ...any) {})
	return nil
}
