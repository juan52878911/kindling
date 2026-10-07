package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/juan52878911/kindling/pkg/lazyre"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// Pieza es un binario instalado que se cambia por un asset de la release.
// La primera de Opciones.Piezas es siempre kling.
type Pieza struct {
	Asset   Asset
	Destino string
}

// Servicio para y arranca el daemon: systemd en Linux, launchd en macOS.
// Parar no mata las microVMs (KillMode=process; en macOS van en su propia
// sesión): el daemon nuevo las readopta al arrancar.
type Servicio interface {
	Parar(ctx context.Context) error
	Arrancar(ctx context.Context) error
	String() string
}

// Daemon es lo que se le pregunta al daemon para saber que el nuevo arrancó
// y que no perdió nada. *api.Client lo cumple.
type Daemon interface {
	Info(ctx context.Context) (*api.Info, error)
	List(ctx context.Context) ([]*api.Machine, error)
	Snapshots(ctx context.Context) ([]*api.Snapshot, error)
}

// Opciones de una actualización o de una vuelta atrás.
type Opciones struct {
	// Dir es donde se bajan los binarios (Dir/<etiqueta>, se borra al acabar)
	// y se guardan las copias (Dir/backups, las dos últimas).
	Dir string
	// Raiz es la raíz de datos del daemon, para comparar esquemas y volver
	// atrás sus migraciones. Vacía = la que diga /info (o nada, sin daemon).
	Raiz string
	// Etiqueta es la release de destino; vacía = la última (o, con
	// Fuente.Dir, la que diga el binario).
	Etiqueta string
	Fuente   *Fuente
	Piezas   []Pieza
	// Servicio y Daemon son nil cuando solo se cambia el CLI (no hay daemon
	// en este host). Entonces Actual es la versión de quien cambia.
	Servicio Servicio
	Daemon   Daemon
	Actual   string

	// Validar comprueba lo bajado (asset -> ruta) antes de tocar nada: en
	// macOS, que kling-vz lleve el permiso de virtualización.
	Validar func(bajados map[string]string) error

	Forzar bool // la misma versión, o una anterior
	EnSeco bool // el plan y nada más
	// Plazo para que el daemon nuevo conteste con su versión. 0 = 60 s.
	Plazo time.Duration
	Out   io.Writer
}

// Resultado es lo que hizo Actualizar.
type Resultado struct {
	Desde, Hacia string
	Copia        string // directorio de la copia de lo de antes
	AlDia        bool   // ya estaba en esa versión: no se tocó nada
	EnSeco       bool
}

// InfoBinario es lo que imprime `kling upgrade -schemas`: la versión de un
// binario y los esquemas que sabe leer. Así el que actualiza pregunta al nuevo
// en vez de suponer.
type InfoBinario struct {
	Kling    string           `json:"kling"`
	API      int              `json:"api"`
	Esquemas machine.Esquemas `json:"schemas"`
	// SinEsquemas es un kling anterior a `upgrade -schemas` (≤ v0.17): leía
	// state.json y los almacenes solo en su versión 0, y los meta.json sin
	// mirar la versión.
	SinEsquemas bool `json:"-"`
}

// ErrVueltaAtras es una actualización que falló después de cambiar los
// binarios y que se deshizo (o no se pudo deshacer: Fallo).
type ErrVueltaAtras struct {
	Causa error
	Fallo error  // nil = se volvió a lo de antes y responde
	Copia string // dónde están los binarios de antes
}

func (e *ErrVueltaAtras) Error() string {
	if e.Fallo == nil {
		return fmt.Sprintf("upgrade failed and was rolled back: %v", e.Causa)
	}
	return fmt.Sprintf("upgrade failed (%v) and so did the rollback (%v): the previous binaries and state are in %s", e.Causa, e.Fallo, e.Copia)
}

func (e *ErrVueltaAtras) Unwrap() error { return e.Causa }

func (o *Opciones) out() io.Writer {
	if o.Out == nil {
		return io.Discard
	}
	return o.Out
}

func (o *Opciones) printf(format string, a ...any) { fmt.Fprintf(o.out(), format, a...) }

// Actualizar cambia las piezas por las de la release y deja el daemon
// corriendo con ellas, o con las de antes si algo falla después de pararlo.
// Hasta parar el daemon, un fallo no ha tocado nada.
func Actualizar(ctx context.Context, o Opciones) (*Resultado, error) {
	if len(o.Piezas) == 0 {
		return nil, errors.New("nothing to upgrade")
	}
	if (o.Servicio == nil) != (o.Daemon == nil) {
		return nil, errors.New("upgrade: a service to restart needs a daemon to check, and the other way round")
	}
	if o.Fuente == nil {
		o.Fuente = &Fuente{}
	}
	res := &Resultado{Desde: o.Actual}
	var antes *foto
	if o.Daemon != nil {
		info, err := o.Daemon.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("the daemon does not answer: %v (kling upgrade replaces a running daemon; start it first)", err)
		}
		res.Desde = info.Version
		if o.Raiz == "" {
			o.Raiz = info.Root
		}
		if antes, err = fotografiar(ctx, o.Daemon); err != nil {
			return nil, err
		}
	}

	etiqueta := o.Etiqueta
	if etiqueta != "" && !EtiquetaValida(etiqueta) {
		return nil, fmt.Errorf("invalid release tag %q (want vX.Y.Z)", etiqueta)
	}
	if etiqueta == "" && o.Fuente.Dir == "" {
		t, err := o.Fuente.UltimaEtiqueta(ctx)
		if err != nil {
			return nil, err
		}
		etiqueta = t
	}
	if etiqueta != "" && mismaVersion(etiqueta, res.Desde) && !o.Forzar {
		res.Hacia, res.AlDia = etiqueta, true
		o.printf("already at %s: nothing to do (-force reinstalls it)\n", res.Desde)
		return res, nil
	}

	// 1-2. Bajar y verificar. Nada ha cambiado todavía.
	nombre := etiqueta
	if nombre == "" {
		nombre = "local"
	}
	staging := filepath.Join(o.Dir, nombre)
	defer os.RemoveAll(staging)
	assets := make([]Asset, len(o.Piezas))
	for i, p := range o.Piezas {
		assets[i] = p.Asset
	}
	origen := "release " + etiqueta
	if o.Fuente.Dir != "" {
		origen = o.Fuente.Dir
	}
	o.printf("downloading and verifying %d file(s) from %s\n", len(assets), origen)
	bajados, err := o.Fuente.Bajar(ctx, etiqueta, assets, staging)
	if err != nil {
		return nil, err
	}

	// 3. Prueba en seco del nuevo: que arranca y qué sabe leer.
	nuevo, err := Sondear(ctx, bajados[o.Piezas[0].Asset.Nombre])
	if err != nil {
		return nil, fmt.Errorf("the new kling does not run here: %v", err)
	}
	res.Hacia = nuevo.Kling
	if etiqueta != "" && !mismaVersion(etiqueta, nuevo.Kling) {
		return nil, fmt.Errorf("the downloaded kling says it is %s, not %s", nuevo.Kling, etiqueta)
	}
	switch {
	case mismaVersion(nuevo.Kling, res.Desde) && !o.Forzar:
		res.AlDia = true
		o.printf("already at %s: nothing to do (-force reinstalls it)\n", res.Desde)
		return res, nil
	case anterior(nuevo.Kling, res.Desde) && !o.Forzar:
		return nil, fmt.Errorf("%s is older than the running %s: going back is kling upgrade -rollback (or -force, if its formats allow it)", nuevo.Kling, res.Desde)
	}

	if o.Validar != nil {
		if err := o.Validar(bajados); err != nil {
			return nil, err
		}
	}

	var migraciones []string
	if o.Raiz != "" {
		disco, err := machine.EsquemasEnDisco(o.Raiz)
		if err != nil {
			return nil, fmt.Errorf("reading the state's formats: %v (run it as the daemon's user)", err)
		}
		soporta := nuevo.Esquemas
		if nuevo.SinEsquemas {
			soporta.Meta = disco.Meta
		}
		if mal := soporta.Incompatibles(disco); len(mal) > 0 {
			return nil, fmt.Errorf("%s cannot use this daemon's data in %s:\n  %s\nnothing was changed", nuevo.Kling, o.Raiz, strings.Join(mal, "\n  "))
		}
		if !nuevo.SinEsquemas {
			migraciones = soporta.Migraciones(disco)
		}
	}

	o.printf("\nplan: %s -> %s\n", res.Desde, nuevo.Kling)
	for _, p := range o.Piezas {
		o.printf("  replace %s\n", p.Destino)
	}
	for _, m := range migraciones {
		o.printf("  migrate %s\n", m)
	}
	if o.Servicio != nil {
		o.printf("  restart %s (running microVMs keep running)\n", o.Servicio)
	}
	if o.EnSeco {
		res.EnSeco = true
		o.printf("\n(dry run: nothing was changed)\n")
		return res, nil
	}

	// 5. Copia de lo de antes.
	copia, err := guardarCopia(o, res.Desde, res.Hacia)
	if err != nil {
		return nil, fmt.Errorf("backing up the current install: %v (nothing was changed)", err)
	}
	res.Copia = copia.dir
	o.printf("\nbackup in %s\n", copia.dir)

	// 6-8. Parar, cambiar, arrancar y verificar; si falla, volver.
	if o.Servicio != nil {
		o.printf("stopping %s\n", o.Servicio)
		if err := o.Servicio.Parar(ctx); err != nil {
			// Puede haberse parado a medias: se arranca lo que hay, que sigue
			// siendo lo de antes.
			_ = o.Servicio.Arrancar(ctx)
			return nil, fmt.Errorf("stopping %s: %v (nothing was changed)", o.Servicio, err)
		}
	}
	causa := func() error {
		for _, p := range o.Piezas {
			if err := colocar(bajados[p.Asset.Nombre], p.Destino); err != nil {
				return fmt.Errorf("installing %s: %v", p.Destino, err)
			}
		}
		if o.Servicio == nil {
			v, err := Sondear(ctx, o.Piezas[0].Destino)
			if err != nil {
				return err
			}
			if v.Kling != nuevo.Kling {
				return fmt.Errorf("%s says %s, want %s", o.Piezas[0].Destino, v.Kling, nuevo.Kling)
			}
			return nil
		}
		o.printf("starting %s\n", o.Servicio)
		if err := o.Servicio.Arrancar(ctx); err != nil {
			return fmt.Errorf("starting %s: %v", o.Servicio, err)
		}
		return o.verificar(ctx, nuevo.Kling, antes)
	}()
	if causa == nil {
		o.printf("upgraded: %s -> %s\n", res.Desde, res.Hacia)
		return res, nil
	}
	o.printf("upgrade failed: %v\nrolling back to %s\n", causa, res.Desde)
	if o.Servicio != nil {
		// Puede estar corriendo (verificar falló) o caído: parado, en todo caso.
		_ = o.Servicio.Parar(ctx)
	}
	fallo := o.restaurar(ctx, copia, true)
	if fallo == nil && o.Servicio != nil {
		fallo = o.esperarVersion(ctx, res.Desde)
	}
	return res, &ErrVueltaAtras{Causa: causa, Fallo: fallo, Copia: copia.dir}
}

// VolverAtras deshace la última actualización con su copia: los binarios de
// antes y los ficheros que el daemon nuevo migró (sus .v<N>.bak vuelven a su
// nombre). Lo creado después en esos ficheros se pierde, como dice
// docs/actualizar.md §6.
func VolverAtras(ctx context.Context, o Opciones) (*Resultado, error) {
	c, err := ultimaCopia(o.Dir)
	if err != nil {
		return nil, err
	}
	res := &Resultado{Desde: c.Hacia, Hacia: c.Desde, Copia: c.dir}
	if o.Raiz == "" {
		o.Raiz = c.Raiz
	}
	viejo, err := Sondear(ctx, filepath.Join(c.dir, c.Piezas[0].Copia))
	if err != nil {
		return nil, fmt.Errorf("the backed-up kling in %s does not run: %v", c.dir, err)
	}
	o.printf("rolling back: %s -> %s (from %s)\n", c.Hacia, viejo.Kling, c.dir)
	for _, p := range c.Piezas {
		o.printf("  restore %s\n", p.Destino)
	}
	for _, b := range bakNuevos(c) {
		o.printf("  restore %s from %s\n", sinBak(b), filepath.Base(b))
	}
	if o.EnSeco {
		res.EnSeco = true
		o.printf("\n(dry run: nothing was changed)\n")
		return res, nil
	}
	if o.Servicio != nil {
		if err := o.Servicio.Parar(ctx); err != nil {
			_ = o.Servicio.Arrancar(ctx)
			return nil, fmt.Errorf("stopping %s: %v (nothing was changed)", o.Servicio, err)
		}
	}
	if err := o.restaurar(ctx, c, false); err != nil {
		return nil, err
	}
	if o.Servicio != nil {
		if err := o.esperarVersion(ctx, viejo.Kling); err != nil {
			return nil, fmt.Errorf("%v; the state.json from before the upgrade is in %s", err, c.dir)
		}
	}
	// Sus binarios ya están en su sitio: el siguiente -rollback es a la
	// copia anterior, no otra vez a esta.
	os.RemoveAll(c.dir)
	o.printf("rolled back to %s\n", viejo.Kling)
	return res, nil
}

// ---- copia de lo de antes

const ficheroCopia = "manifest.json"

// copia es Dir/backups/<fecha>-<versión>/manifest.json y lo que lista.
type copia struct {
	dir    string
	Desde  string       `json:"from"`
	Hacia  string       `json:"to"`
	Creada time.Time    `json:"created"`
	Raiz   string       `json:"root,omitempty"`
	Piezas []piezaCopia `json:"files"`
	Estado string       `json:"state,omitempty"` // copia de state.json, si había
	Baks   []string     `json:"baks_before,omitempty"`
	Extras []piezaCopia `json:"extra,omitempty"` // extensiones (AñadirACopia)
}

type piezaCopia struct {
	Destino string `json:"path"`
	Copia   string `json:"copy"` // nombre dentro del directorio de la copia
}

// copiasGuardadas es cuántas copias se conservan: la de esta actualización y
// la de la anterior (§3.4).
const copiasGuardadas = 2

func guardarCopia(o Opciones, desde, hacia string) (*copia, error) {
	base := filepath.Join(o.Dir, "backups")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(base, time.Now().UTC().Format("20060102T150405.000Z")+"-"+nombreSeguro(desde))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	c := &copia{dir: dir, Desde: desde, Hacia: hacia, Creada: time.Now().UTC(), Raiz: o.Raiz}
	falla := func(err error) (*copia, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	for i, p := range o.Piezas {
		nombre := fmt.Sprintf("%d-%s", i, filepath.Base(p.Destino))
		if err := colocar(p.Destino, filepath.Join(dir, nombre)); err != nil {
			return falla(err)
		}
		c.Piezas = append(c.Piezas, piezaCopia{Destino: p.Destino, Copia: nombre})
	}
	if o.Raiz != "" {
		est := filepath.Join(o.Raiz, "state.json")
		if _, err := os.Stat(est); err == nil {
			if err := colocar(est, filepath.Join(dir, "state.json")); err != nil {
				return falla(err)
			}
			c.Estado = "state.json"
		}
		c.Baks = baks(o.Raiz)
	}
	if err := c.escribir(); err != nil {
		return falla(err)
	}
	podarCopias(base, copiasGuardadas)
	return c, nil
}

func (c *copia) escribir() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.dir, ficheroCopia+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.dir, ficheroCopia))
}

// AñadirACopia guarda en la última copia de dir los ficheros que se van a
// cambiar fuera de Actualizar (las extensiones, que se actualizan después del
// núcleo), para que -rollback también los devuelva.
func AñadirACopia(dir string, rutas []string) error {
	c, err := ultimaCopia(dir)
	if err != nil {
		return err
	}
	for _, p := range rutas {
		nombre := fmt.Sprintf("x%d-%s", len(c.Extras), filepath.Base(p))
		if err := colocar(p, filepath.Join(c.dir, nombre)); err != nil {
			return err
		}
		c.Extras = append(c.Extras, piezaCopia{Destino: p, Copia: nombre})
	}
	return c.escribir()
}

func ultimaCopia(dir string) (*copia, error) {
	base := filepath.Join(dir, "backups")
	ents, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var nombres []string
	for _, e := range ents {
		if e.IsDir() {
			nombres = append(nombres, e.Name())
		}
	}
	sort.Strings(nombres)
	for i := len(nombres) - 1; i >= 0; i-- {
		d := filepath.Join(base, nombres[i])
		b, err := os.ReadFile(filepath.Join(d, ficheroCopia))
		if err != nil {
			continue // a medias: guardarCopia no llegó a escribir el manifiesto
		}
		c := &copia{dir: d}
		if err := json.Unmarshal(b, c); err != nil || len(c.Piezas) == 0 {
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("no upgrade backup in %s: nothing to roll back to", base)
}

func podarCopias(base string, guardar int) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var nombres []string
	for _, e := range ents {
		if e.IsDir() {
			nombres = append(nombres, e.Name())
		}
	}
	sort.Strings(nombres)
	for len(nombres) > guardar {
		os.RemoveAll(filepath.Join(base, nombres[0]))
		nombres = nombres[1:]
	}
}

// restaurar devuelve a su sitio lo de la copia y arranca el daemon, que quien
// llama ya ha parado. conEstado vuelve también el state.json guardado: solo tras
// un fallo recién ocurrido, cuando nada nuevo ha podido entrar en él.
func (o *Opciones) restaurar(ctx context.Context, c *copia, conEstado bool) error {
	var errs []error
	for _, p := range append(append([]piezaCopia(nil), c.Piezas...), c.Extras...) {
		if err := colocar(filepath.Join(c.dir, p.Copia), p.Destino); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %v", p.Destino, err))
		}
	}
	// Lo que el daemon nuevo migró deja su .v<N>.bak: vuelve a su nombre y la
	// copia se quita, para que la próxima migración la haga de nuevo.
	for _, b := range bakNuevos(c) {
		if err := colocar(b, sinBak(b)); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %v", sinBak(b), err))
			continue
		}
		os.Remove(b)
	}
	if conEstado && c.Estado != "" && c.Raiz != "" {
		if err := colocar(filepath.Join(c.dir, c.Estado), filepath.Join(c.Raiz, "state.json")); err != nil {
			errs = append(errs, fmt.Errorf("restoring state.json: %v", err))
		}
	}
	if o.Servicio != nil {
		if err := o.Servicio.Arrancar(ctx); err != nil {
			errs = append(errs, fmt.Errorf("starting %s: %v", o.Servicio, err))
		}
	}
	return errors.Join(errs...)
}

// reBak es la copia que deja una migración (pkg/esquema.RutaRespaldo).
var reBak = lazyre.New(`\.v[0-9]+\.bak$`)

func sinBak(p string) string { return reBak.ReplaceAllString(p, "") }

// baks lista las copias de migración que hay en la raíz: las de state.json,
// los meta.json de los dorados y los almacenes de credenciales.
func baks(raiz string) []string {
	var out []string
	for _, patron := range []string{
		filepath.Join(raiz, "state.json.v*.bak"),
		filepath.Join(raiz, "snapshots", "*", "meta.json.v*.bak"),
		filepath.Join(raiz, "machines", "*", "credentials.enc.v*.bak"),
		filepath.Join(raiz, "secrets", "credentials", "*.enc.v*.bak"),
		filepath.Join(raiz, "store", "graph", "*.secrets.enc.v*.bak"),
	} {
		m, _ := filepath.Glob(patron)
		for _, p := range m {
			if reBak.MatchString(p) {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// bakNuevos son las copias de migración que no estaban al hacer la copia: lo
// que migró el daemon nuevo.
func bakNuevos(c *copia) []string {
	if c.Raiz == "" {
		return nil
	}
	habia := map[string]bool{}
	for _, b := range c.Baks {
		habia[b] = true
	}
	var out []string
	for _, b := range baks(c.Raiz) {
		if !habia[b] {
			out = append(out, b)
		}
	}
	return out
}

// colocar deja una copia de src en dst sin que dst exista a medias en ningún
// momento: temporal en el directorio de dst y rename encima. El proceso que
// esté ejecutando dst sigue con su inodo (sobrescribirlo en su sitio mata a
// un binario firmado en macOS). Conserva el modo de dst si existía.
func colocar(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	modo := os.FileMode(0o755)
	if st, err := os.Stat(dst); err == nil {
		modo = st.Mode().Perm()
	} else if st, err := in.Stat(); err == nil {
		modo = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(modo); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func nombreSeguro(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ' ' {
			return '_'
		}
		return r
	}, v)
}

// ---- el binario nuevo

// Sondear ejecuta un kling y le pregunta su versión y sus esquemas (`upgrade
// -schemas`). Uno anterior a `kling upgrade` no lo sabe: se le pide `version`
// y se supone lo que leía v0.17 (esquemas 0; los meta.json los leía sin
// mirar la versión). Corre sin hablar con ningún daemon.
func Sondear(ctx context.Context, bin string) (*InfoBinario, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	correr := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = entornoSinDaemon()
		var errb bytes.Buffer
		cmd.Stderr = &errb
		out, err := cmd.Output()
		if err != nil {
			if s := strings.TrimSpace(errb.String()); s != "" {
				return nil, fmt.Errorf("%v: %s", err, primeraLinea(s))
			}
			return nil, err
		}
		return out, nil
	}
	if out, err := correr("upgrade", "-schemas"); err == nil {
		var ib InfoBinario
		if json.Unmarshal(out, &ib) == nil && ib.Kling != "" {
			return &ib, nil
		}
	}
	out, err := correr("version")
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	if sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "kling" {
			return &InfoBinario{Kling: f[1], SinEsquemas: true}, nil
		}
	}
	return nil, fmt.Errorf("%s version: unexpected output %q", bin, primeraLinea(string(out)))
}

// entornoSinDaemon es el entorno de quien actualiza con KLING_HOST apuntando
// a ninguna parte: `kling version` no debe preguntar a un daemon (ni abrir un
// ssh si el contexto activo es remoto).
func entornoSinDaemon() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KLING_HOST=") {
			env = append(env, kv)
		}
	}
	return append(env, "KLING_HOST=unix:///nonexistent/kling-upgrade.sock")
}

func primeraLinea(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// mismaVersion compara dos versiones como cadenas, sin la "v".
func mismaVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// anterior dice si a es una versión estrictamente anterior a b por sus tres
// números. Un build de desarrollo de la misma (v0.17.0-12-gabc) no lo es.
func anterior(a, b string) bool {
	return plugin.VersionAtLeast(b, a) && !plugin.VersionAtLeast(a, b)
}

// ---- verificación

// foto es lo que había antes: las máquinas y su estado, y los dorados.
type foto struct {
	maquinas map[string]api.State
	dorados  map[string]bool
}

func fotografiar(ctx context.Context, d Daemon) (*foto, error) {
	ms, err := d.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing machines: %v", err)
	}
	ss, err := d.Snapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing templates: %v", err)
	}
	f := &foto{maquinas: map[string]api.State{}, dorados: map[string]bool{}}
	for _, m := range ms {
		f.maquinas[m.ID] = m.State
	}
	for _, s := range ss {
		f.dorados[s.Name] = true
	}
	return f, nil
}

func (o *Opciones) plazo() time.Duration {
	if o.Plazo <= 0 {
		return 60 * time.Second
	}
	return o.Plazo
}

// esperarVersion espera a que el daemon conteste con la versión v.
func (o *Opciones) esperarVersion(ctx context.Context, v string) error {
	ctx, cancel := context.WithTimeout(ctx, o.plazo())
	defer cancel()
	var ultimo string
	for {
		ictx, icancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := o.Daemon.Info(ictx)
		icancel()
		switch {
		case err == nil && mismaVersion(info.Version, v):
			return nil
		case err == nil:
			ultimo = "it answers as " + info.Version
		default:
			ultimo = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the daemon did not come back as %s within %s (%s)", v, o.plazo(), ultimo)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// verificar comprueba que el daemon nuevo contesta con su versión y que no
// perdió nada: las máquinas siguen, las congeladas siguen congeladas y los
// dorados siguen listados.
func (o *Opciones) verificar(ctx context.Context, v string, antes *foto) error {
	if err := o.esperarVersion(ctx, v); err != nil {
		return err
	}
	despues, err := fotografiar(ctx, o.Daemon)
	if err != nil {
		return err
	}
	var falta []string
	for id, st := range antes.maquinas {
		ahora, ok := despues.maquinas[id]
		switch {
		case !ok:
			falta = append(falta, "machine "+id+" is gone")
		case st == api.StateWarm && ahora != api.StateWarm:
			falta = append(falta, fmt.Sprintf("machine %s was frozen and is now %s", id, ahora))
		}
	}
	for n := range antes.dorados {
		if !despues.dorados[n] {
			falta = append(falta, "template "+n+" is gone")
		}
	}
	if len(falta) > 0 {
		sort.Strings(falta)
		return fmt.Errorf("the new daemon lost things: %s", strings.Join(falta, "; "))
	}
	return nil
}
