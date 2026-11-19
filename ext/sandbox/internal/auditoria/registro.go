// Package auditoria escribe, por inquilino, una línea JSON por cada acción con
// efecto que pasa por el gateway (crear/borrar sandboxes, exec, ficheros,
// procesos, sesiones, endpoints, claves) en un fichero rotado por tamaño.
//
// El formato sigue el de credaudit del núcleo (pkg/credproxy/auditoria.go): un
// JSON por línea, escritura asíncrona por una sola goroutine con un canal que
// no bloquea (si no cabe, se descarta y la cuenta viaja en el campo dropped de
// la siguiente línea), rotación a .1, .2... y fichero abierto con O_NOFOLLOW
// y 0600. Qué NO se escribe nunca: tokens, claves de inquilino, valores de
// variables de entorno, contenido de ficheros ni cuerpos de respuesta.
package auditoria

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// Fichero es el nombre del registro dentro de -audit-dir.
	Fichero = "audit.jsonl"
	// MaxBytesPorDefecto es el tamaño a partir del cual se rota.
	MaxBytesPorDefecto = 8 << 20
	// GeneracionesPorDefecto es cuántos ficheros rotados (.1, .2...) se guardan.
	GeneracionesPorDefecto = 5
	maxGeneraciones        = 99
	colaPorDefecto         = 1024
)

// Linea es una acción auditada.
type Linea struct {
	Ts         time.Time      `json:"ts"`
	Tenant     string         `json:"tenant"`
	Accion     string         `json:"action"`
	Sandbox    string         `json:"sandbox,omitempty"`
	Resultado  string         `json:"result"` // ok | denied | error
	Estado     int            `json:"status"`
	DuracionMs int64          `json:"duration_ms"`
	Traza      string         `json:"trace_id"`
	Detalle    map[string]any `json:"detail,omitempty"`
	// Descartados es cuántas líneas se perdieron justo antes de esta.
	Descartados int64 `json:"dropped,omitempty"`
}

// Opciones del registro.
type Opciones struct {
	Dir          string
	MaxBytes     int64
	Generaciones int
	Cola         int
}

type item struct {
	linea Linea
	sync  chan struct{}
}

// Registro es el escritor rotado.
type Registro struct {
	o       Opciones
	ch      chan item
	hecho   chan struct{}
	pend    atomic.Int64 // descartados aún sin anotar en una línea
	total   atomic.Int64
	cierra  sync.Once
	cerrado atomic.Bool
	mu      sync.RWMutex // protege la rotación frente a Leer

	f    *os.File
	size int64
}

// Abrir crea el directorio (0700) y el fichero (0600) y arranca el escritor.
func Abrir(o Opciones) (*Registro, error) {
	if o.Dir == "" {
		return nil, errors.New("audit dir is empty")
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = MaxBytesPorDefecto
	}
	if o.Generaciones <= 0 {
		o.Generaciones = GeneracionesPorDefecto
	}
	if o.Generaciones > maxGeneraciones {
		return nil, fmt.Errorf("audit generations can't pass %d", maxGeneraciones)
	}
	if o.Cola <= 0 {
		o.Cola = colaPorDefecto
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	r := &Registro{o: o, ch: make(chan item, o.Cola), hecho: make(chan struct{})}
	if err := r.abrir(); err != nil {
		return nil, err
	}
	go r.bucle()
	return r, nil
}

func (r *Registro) ruta(n int) string {
	p := filepath.Join(r.o.Dir, Fichero)
	if n > 0 {
		p += "." + strconv.Itoa(n)
	}
	return p
}

// abrir abre el fichero actual sin seguir enlaces y exige que sea regular y 0600.
func (r *Registro) abrir() error {
	f, err := os.OpenFile(r.ruta(0), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return errors.New("audit file is not a regular file")
	}
	if st.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return err
		}
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Escribir deja la línea en cola sin bloquear; con la cola llena la descarta y
// la cuenta.
func (r *Registro) Escribir(l Linea) {
	if r.cerrado.Load() {
		return
	}
	select {
	case r.ch <- item{linea: l}:
	default:
		r.pend.Add(1)
		r.total.Add(1)
	}
}

// Descartados es el total de líneas perdidas desde que se abrió.
func (r *Registro) Descartados() int64 { return r.total.Load() }

// Vaciar espera a que todo lo encolado antes esté en el fichero.
func (r *Registro) Vaciar() {
	if r.cerrado.Load() {
		return
	}
	c := make(chan struct{})
	select {
	case r.ch <- item{sync: c}:
		select {
		case <-c:
		case <-r.hecho:
		}
	case <-r.hecho:
	}
}

// Cerrar vacía y cierra el fichero.
func (r *Registro) Cerrar() {
	r.cierra.Do(func() {
		r.cerrado.Store(true)
		close(r.ch)
		<-r.hecho
	})
}

func (r *Registro) bucle() {
	defer close(r.hecho)
	for it := range r.ch {
		if it.sync != nil {
			close(it.sync)
			continue
		}
		l := it.linea
		if n := r.pend.Swap(0); n > 0 {
			l.Descartados += n
		}
		b, err := json.Marshal(l)
		if err != nil {
			continue
		}
		b = append(b, '\n')
		r.mu.Lock()
		if r.size > 0 && r.size+int64(len(b)) > r.o.MaxBytes {
			r.rotar()
		}
		if r.f != nil {
			n, err := r.f.Write(b)
			r.size += int64(n)
			if err != nil {
				r.pend.Add(1)
				r.total.Add(1)
			}
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	if r.f != nil {
		r.f.Close()
	}
	r.mu.Unlock()
}

// rotar desplaza .N-1 a .N... y el actual a .1; la última generación se cae.
func (r *Registro) rotar() {
	r.f.Close()
	r.f = nil
	_ = os.Remove(r.ruta(r.o.Generaciones))
	for n := r.o.Generaciones - 1; n >= 1; n-- {
		_ = os.Rename(r.ruta(n), r.ruta(n+1))
	}
	_ = os.Rename(r.ruta(0), r.ruta(1))
	if err := r.abrir(); err != nil {
		r.f = nil
	}
}

// Leer devuelve, de más antigua a más reciente, las líneas con ts >= desde (y
// del tenant dado, si no es ""), hasta limite. El segundo valor dice si había más.
func (r *Registro) Leer(desde time.Time, tenant string, limite int) ([]Linea, bool, error) {
	r.Vaciar()
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Linea
	for n := r.o.Generaciones; n >= 0; n-- {
		f, err := os.Open(r.ruta(n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		sc := bufio.NewReader(f)
		for {
			ln, err := sc.ReadBytes('\n')
			if len(ln) > 0 {
				var l Linea
				if json.Unmarshal(ln, &l) == nil && !l.Ts.Before(desde) && (tenant == "" || l.Tenant == tenant) {
					if limite > 0 && len(out) >= limite {
						f.Close()
						return out, true, nil
					}
					out = append(out, l)
				}
			}
			if err != nil {
				if err != io.EOF {
					f.Close()
					return nil, false, err
				}
				break
			}
		}
		f.Close()
	}
	return out, false, nil
}
