// Package inquilinos es el almacén persistente de inquilinos y sus claves.
//
// Los inquilinos de KLING_SANDBOX_TENANTS son estáticos: sin rotación ni
// revocación sin reiniciar. Este almacén los complementa: un fichero JSON en el
// directorio de estado del gateway con los inquilinos (nombre, topes) y sus
// claves. De una clave solo se guarda un hash con sal: la clave en claro se
// entrega UNA vez, al crearla o rotarla, y no se puede recuperar.
//
// Formato de clave: "ksk_" + 12 hex (id de la clave, visible) + "_" + secreto
// (32 bytes aleatorios en base64 url). El id permite buscar la clave sin
// recorrerlas todas; la comparación del hash es en tiempo constante.
//
// El nombre del inquilino es su id y no cambia: los sandboxes llevan el nombre
// en una etiqueta, así que renombrar dejaría huérfanos los que ya existen.
package inquilinos

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Errores que el frontal traduce a códigos HTTP.
var (
	ErrNoExiste      = errors.New("tenant not found")
	ErrExiste        = errors.New("tenant already exists")
	ErrClaveNoExiste = errors.New("key not found")
	ErrNombre        = errors.New("tenant name is not valid (letters, digits, '.', '_', '-')")
	ErrTope          = errors.New("limits can't be negative")
	ErrGracia        = errors.New("grace period out of range")
	ErrPasado        = errors.New("expires_at is in the past")
)

// Fichero es el nombre del fichero dentro del directorio de estado.
const Fichero = "tenants.json"

// MaxGracia es la mayor gracia de una rotación.
const MaxGracia = 24 * time.Hour

var nombreValido = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Topes son los límites de un inquilino; 0 = el valor por defecto del frontal.
type Topes struct {
	MaxSandboxes    int `json:"max_sandboxes,omitempty"`
	MaxPorPlantilla int `json:"max_per_template,omitempty"`
	MaxShells       int `json:"max_shells,omitempty"`
	MaxProcesos     int `json:"max_processes,omitempty"`
	MaxSesiones     int `json:"max_sessions,omitempty"`
	MaxSeguir       int `json:"max_follow,omitempty"`
	MaxMemMiB       int `json:"max_mem_mib,omitempty"`
	MaxVCPUs        int `json:"max_vcpus,omitempty"`
	MaxMemTotalMiB  int `json:"max_mem_total_mib,omitempty"`
	MaxLote         int `json:"max_batch,omitempty"`
	MaxVolumenes    int `json:"max_volumes,omitempty"`
}

func (t Topes) valido() bool {
	for _, n := range []int{t.MaxSandboxes, t.MaxPorPlantilla, t.MaxShells, t.MaxProcesos, t.MaxSesiones,
		t.MaxSeguir, t.MaxMemMiB, t.MaxVCPUs, t.MaxMemTotalMiB, t.MaxLote, t.MaxVolumenes} {
		if n < 0 {
			return false
		}
	}
	return true
}

// Clave es una clave de acceso. Sal y Hash nunca salen del paquete salvo al
// fichero.
type Clave struct {
	ID        string     `json:"id"`
	Prefijo   string     `json:"prefix"`
	Sal       string     `json:"salt"`
	Hash      string     `json:"hash"`
	Creada    time.Time  `json:"created"`
	Caduca    *time.Time `json:"expires,omitempty"`
	Revocada  *time.Time `json:"revoked,omitempty"`
	UltimoUso *time.Time `json:"last_used,omitempty"`
}

// Inquilino es un inquilino con sus claves.
type Inquilino struct {
	Nombre string    `json:"name"`
	Admin  bool      `json:"admin,omitempty"`
	Topes  Topes     `json:"limits"`
	Creado time.Time `json:"created"`
	Claves []Clave   `json:"keys"`
}

type estado struct {
	Version    int         `json:"version"`
	Inquilinos []Inquilino `json:"tenants"`
}

func (e *estado) clonar() *estado {
	c := &estado{Version: e.Version, Inquilinos: make([]Inquilino, len(e.Inquilinos))}
	for i, t := range e.Inquilinos {
		t.Claves = append([]Clave(nil), t.Claves...)
		c.Inquilinos[i] = t
	}
	return c
}

func (e *estado) buscar(nombre string) int {
	for i := range e.Inquilinos {
		if e.Inquilinos[i].Nombre == nombre {
			return i
		}
	}
	return -1
}

type ubicacion struct{ t, c int }

// Opciones del almacén.
type Opciones struct {
	// CadaUso: cada cuánto se vuelca a disco el "último uso" (0 = 1 min).
	CadaUso time.Duration
	// Ahora sustituye a time.Now en los tests.
	Ahora func() time.Time
}

// Almacen es el almacén. Seguro para uso concurrente.
type Almacen struct {
	ruta string
	op   Opciones

	mu      sync.Mutex
	est     *estado
	indice  map[string]ubicacion // id de clave → posición; se rehace al escribir
	volcado time.Time
	// gen cuenta las versiones del estado en memoria (con a.mu); escrita es
	// la última que llegó a disco (con a.esc). gen por delante de escrita: hay
	// "último uso" sin volcar.
	gen uint64

	// esc ordena las escrituras del fichero. Se toma después de a.mu (mutar
	// la toma con a.mu tomado) o sin a.mu (el volcado del último uso, que no
	// para a las autenticaciones mientras hace fsync).
	esc     sync.Mutex
	escrita uint64
}

// Abrir carga (o crea) el almacén en dir.
func Abrir(dir string, op Opciones) (*Almacen, error) {
	if op.Ahora == nil {
		op.Ahora = time.Now
	}
	if op.CadaUso == 0 {
		op.CadaUso = time.Minute
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &Almacen{ruta: filepath.Join(dir, Fichero), op: op, est: &estado{Version: 1}}
	b, err := os.ReadFile(a.ruta)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, a.est); err != nil {
			return nil, fmt.Errorf("%s: %w", a.ruta, err)
		}
		// Un fichero con permisos de más (restaurado de una copia, por
		// ejemplo) se aprieta en vez de aceptarlo.
		if err := os.Chmod(a.ruta, 0o600); err != nil {
			return nil, err
		}
	}
	a.reindexar()
	a.volcado = op.Ahora()
	return a, nil
}

func (a *Almacen) reindexar() {
	a.indice = map[string]ubicacion{}
	for i, t := range a.est.Inquilinos {
		for j, c := range t.Claves {
			a.indice[c.ID] = ubicacion{i, j}
		}
	}
}

// guardar escribe e atómicamente: temporal en el mismo directorio, fsync,
// rename y fsync del directorio. Permisos 0600.
func (a *Almacen) guardar(e *estado) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.ruta)
	f, err := os.CreateTemp(dir, ".tenants-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fallo := func(err error) error { f.Close(); os.Remove(tmp); return err }
	if err := f.Chmod(0o600); err != nil {
		return fallo(err)
	}
	if _, err := f.Write(b); err != nil {
		return fallo(err)
	}
	if err := f.Sync(); err != nil {
		return fallo(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, a.ruta); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// mutar aplica f a una copia, la escribe y, si todo va bien, la adopta.
func (a *Almacen) mutar(f func(e *estado) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.est.clonar()
	if err := f(c); err != nil {
		return err
	}
	g := a.gen + 1
	if err := a.escribir(c, g); err != nil {
		return err
	}
	a.est, a.gen, a.volcado = c, g, a.op.Ahora()
	a.reindexar()
	return nil
}

// escribir guarda e, que es la versión g del estado, salvo que en disco ya
// haya una igual o más nueva: un volcado del último uso que se quedó atrás no
// pisa lo que escribió un cambio posterior (una revocación, por ejemplo).
func (a *Almacen) escribir(e *estado, g uint64) error {
	a.esc.Lock()
	defer a.esc.Unlock()
	return a.escribirLocked(e, g)
}

// escribirLocked es escribir con a.esc ya tomado.
func (a *Almacen) escribirLocked(e *estado, g uint64) error {
	if g <= a.escrita {
		return nil
	}
	if err := a.guardar(e); err != nil {
		return err
	}
	a.escrita = g
	return nil
}

func aleatorio(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // sin entropía no hay nada seguro que hacer
	}
	return b
}

func hash(sal []byte, secreto string) []byte {
	h := sha256.New()
	h.Write(sal)
	h.Write([]byte(secreto))
	return h.Sum(nil)
}

const (
	pre     = "ksk_"
	largoID = 12
)

// nuevaClave devuelve la clave guardable y el texto en claro.
func (a *Almacen) nuevaClave(e *estado, caduca *time.Time) (Clave, string) {
	var id string
	for {
		id = hex.EncodeToString(aleatorio(largoID / 2))
		if _, ok := a.indice[id]; ok {
			continue
		}
		dup := false
		for _, t := range e.Inquilinos {
			for _, c := range t.Claves {
				dup = dup || c.ID == id
			}
		}
		if !dup {
			break
		}
	}
	secreto := base64.RawURLEncoding.EncodeToString(aleatorio(32))
	sal := aleatorio(16)
	c := Clave{
		ID: id, Prefijo: pre + id,
		Sal: hex.EncodeToString(sal), Hash: hex.EncodeToString(hash(sal, secreto)),
		Creada: a.op.Ahora().UTC(), Caduca: caduca,
	}
	return c, pre + id + "_" + secreto
}

// Cuantos dice cuántos inquilinos hay.
func (a *Almacen) Cuantos() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.est.Inquilinos)
}

func copiar(t Inquilino) Inquilino {
	t.Claves = append([]Clave(nil), t.Claves...)
	return t
}

// Listar devuelve copias de todos los inquilinos.
func (a *Almacen) Listar() []Inquilino {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Inquilino, len(a.est.Inquilinos))
	for i, t := range a.est.Inquilinos {
		out[i] = copiar(t)
	}
	return out
}

// Obtener devuelve una copia del inquilino.
func (a *Almacen) Obtener(nombre string) (Inquilino, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := a.est.buscar(nombre); i >= 0 {
		return copiar(a.est.Inquilinos[i]), nil
	}
	return Inquilino{}, ErrNoExiste
}

// Crear añade un inquilino sin claves.
func (a *Almacen) Crear(nombre string, admin bool, t Topes) (Inquilino, error) {
	if !nombreValido.MatchString(nombre) {
		return Inquilino{}, ErrNombre
	}
	if !t.valido() {
		return Inquilino{}, ErrTope
	}
	nuevo := Inquilino{Nombre: nombre, Admin: admin, Topes: t, Creado: a.op.Ahora().UTC(), Claves: []Clave{}}
	err := a.mutar(func(e *estado) error {
		if e.buscar(nombre) >= 0 {
			return ErrExiste
		}
		e.Inquilinos = append(e.Inquilinos, nuevo)
		return nil
	})
	return nuevo, err
}

// Actualizar cambia los campos no nulos.
func (a *Almacen) Actualizar(nombre string, admin *bool, t *Topes) (Inquilino, error) {
	if t != nil && !t.valido() {
		return Inquilino{}, ErrTope
	}
	var out Inquilino
	err := a.mutar(func(e *estado) error {
		i := e.buscar(nombre)
		if i < 0 {
			return ErrNoExiste
		}
		if admin != nil {
			e.Inquilinos[i].Admin = *admin
		}
		if t != nil {
			e.Inquilinos[i].Topes = *t
		}
		out = copiar(e.Inquilinos[i])
		return nil
	})
	return out, err
}

// Borrar quita el inquilino y con él todas sus claves.
func (a *Almacen) Borrar(nombre string) error {
	return a.mutar(func(e *estado) error {
		i := e.buscar(nombre)
		if i < 0 {
			return ErrNoExiste
		}
		e.Inquilinos = append(e.Inquilinos[:i], e.Inquilinos[i+1:]...)
		return nil
	})
}

// NuevaClave crea una clave del inquilino. Devuelve su ficha y el texto en
// claro, que no se vuelve a poder leer.
func (a *Almacen) NuevaClave(nombre string, caduca *time.Time) (Clave, string, error) {
	var c Clave
	var texto string
	if caduca != nil && !caduca.After(a.op.Ahora()) {
		return Clave{}, "", ErrPasado
	}
	err := a.mutar(func(e *estado) error {
		i := e.buscar(nombre)
		if i < 0 {
			return ErrNoExiste
		}
		c, texto = a.nuevaClave(e, caduca)
		e.Inquilinos[i].Claves = append(e.Inquilinos[i].Claves, c)
		return nil
	})
	if err != nil {
		return Clave{}, "", err
	}
	return c, texto, nil
}

// Rotar crea una clave nueva y deja la vieja válida solo durante gracia. Si la
// vieja ya caducaba antes, se respeta ese plazo; la nueva hereda su caducidad.
func (a *Almacen) Rotar(nombre, kid string, gracia time.Duration) (nueva Clave, texto string, vieja Clave, err error) {
	if gracia < 0 || gracia > MaxGracia {
		return Clave{}, "", Clave{}, ErrGracia
	}
	err = a.mutar(func(e *estado) error {
		i := e.buscar(nombre)
		if i < 0 {
			return ErrNoExiste
		}
		j := -1
		for k, c := range e.Inquilinos[i].Claves {
			if c.ID == kid {
				j = k
			}
		}
		now := a.op.Ahora().UTC()
		if j < 0 || e.Inquilinos[i].Claves[j].Revocada != nil || caducada(e.Inquilinos[i].Claves[j], now) {
			return ErrClaveNoExiste
		}
		v := &e.Inquilinos[i].Claves[j]
		nueva, texto = a.nuevaClave(e, v.Caduca)
		fin := now.Add(gracia)
		if v.Caduca == nil || fin.Before(*v.Caduca) {
			v.Caduca = &fin
		}
		vieja = *v
		e.Inquilinos[i].Claves = append(e.Inquilinos[i].Claves, nueva)
		return nil
	})
	if err != nil {
		return Clave{}, "", Clave{}, err
	}
	return nueva, texto, vieja, nil
}

// caducada dice si la clave ya caducó.
func caducada(c Clave, now time.Time) bool { return c.Caduca != nil && !now.Before(*c.Caduca) }

// Revocar invalida la clave al instante. Revocar una revocada no falla.
func (a *Almacen) Revocar(nombre, kid string) error {
	return a.mutar(func(e *estado) error {
		i := e.buscar(nombre)
		if i < 0 {
			return ErrNoExiste
		}
		for j := range e.Inquilinos[i].Claves {
			if c := &e.Inquilinos[i].Claves[j]; c.ID == kid {
				if c.Revocada == nil {
					t := a.op.Ahora().UTC()
					c.Revocada = &t
				}
				return nil
			}
		}
		return ErrClaveNoExiste
	})
}

// senuelo es contra lo que se compara cuando la clave no existe, para que el
// tiempo no delate si el id era bueno.
var senuelo = make([]byte, sha256.Size)

// Resolver dice a qué inquilino abre el texto de una clave. Anota el último uso
// en memoria; se vuelca a disco como mucho cada Opciones.CadaUso.
func (a *Almacen) Resolver(texto string) (Inquilino, bool) {
	var id, secreto string
	bien := len(texto) > len(pre)+largoID+1 && texto[:len(pre)] == pre && texto[len(pre)+largoID] == '_'
	if bien {
		id, secreto = texto[len(pre):len(pre)+largoID], texto[len(pre)+largoID+1:]
	}
	a.mu.Lock()
	var cl *Clave
	var ti int
	if bien {
		if u, ok := a.indice[id]; ok {
			ti, cl = u.t, &a.est.Inquilinos[u.t].Claves[u.c]
		}
	}
	esperado, sal := senuelo, senuelo
	if cl != nil {
		esperado, _ = hex.DecodeString(cl.Hash)
		sal, _ = hex.DecodeString(cl.Sal)
	}
	coincide := subtle.ConstantTimeCompare(hash(sal, secreto), esperado) == 1
	if cl == nil || !coincide {
		a.mu.Unlock()
		return Inquilino{}, false
	}
	now := a.op.Ahora().UTC()
	if cl.Revocada != nil || caducada(*cl, now) {
		a.mu.Unlock()
		return Inquilino{}, false
	}
	cl.UltimoUso = &now
	a.gen++
	out := copiar(a.est.Inquilinos[ti])
	// El volcado se prepara con el cerrojo (una copia del estado) y se escribe
	// sin él (D-06): el fsync ya no para a las demás autenticaciones. Solo lo
	// paga la petición a la que le toca, una vez por Opciones.CadaUso.
	var copia *estado
	var g uint64
	if now.Sub(a.volcado) >= a.op.CadaUso {
		copia, g = a.est.clonar(), a.gen
		a.volcado = a.op.Ahora()
	}
	a.mu.Unlock()
	if copia != nil && a.esc.TryLock() {
		// Si ya se está escribiendo, no se hace cola detrás: gen sigue por
		// delante de lo escrito y el volcado siguiente lo recoge. Un fallo
		// solo pierde el "último uso", igual que antes.
		_ = a.escribirLocked(copia, g)
		a.esc.Unlock()
	}
	return out, true
}

// Cerrar vuelca lo pendiente. escribir espera al volcado de Resolver que
// pudiera estar a medias y no reescribe si ese ya dejó en disco lo último.
func (a *Almacen) Cerrar() {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.escribir(a.est, a.gen)
	a.volcado = a.op.Ahora()
}
