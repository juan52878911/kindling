package durable

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EL PATRÓN «TEMPORAL + HASH + FSYNC + RENAME». Había cinco copias (la
// descarga de blobs OCI, imagen.FetchVerified, las dos subidas de blobs del
// daemon y la copia a la caché verificada del constructor) y cada una daba
// garantías distintas: una con un temporal de nombre fijo que dos escritores
// se pisaban, otra sin fsync antes del rename, otras sin fsync del
// directorio. Aquí, una sola vez:
//
//   - un temporal propio por escritura (CreateTemp) en el directorio del
//     destino, para que el rename sea atómico y dos a la vez no se mezclen;
//   - el sha256 y el tamaño de lo que se escribió, comprobados antes de
//     publicar nada;
//   - el contenido en disco antes del rename (Ordenar) y el directorio
//     después, como Escribir: existir en dst implica estar entero y ser lo
//     que se verificó.

// ErrSHA256 es el de un contenido que no tiene el sha256 esperado.
type ErrSHA256 struct{ Got, Want string }

func (e *ErrSHA256) Error() string {
	return fmt.Sprintf("sha256 mismatch: got %s, want %s", e.Got, e.Want)
}

// ErrTamaño es el de un contenido que no mide lo declarado.
type ErrTamaño struct{ N, Quiero int64 }

func (e *ErrTamaño) Error() string {
	return fmt.Sprintf("size %d, expected %d", e.N, e.Quiero)
}

// ErrDemasiado es el de un contenido que pasa del tope.
type ErrDemasiado struct{ Max int64 }

func (e *ErrDemasiado) Error() string { return fmt.Sprintf("over %d bytes", e.Max) }

// ErrLectura envuelve un fallo al leer lo que llega (la red, el cliente):
// no es del disco, y quien llama puede contestar distinto.
type ErrLectura struct{ Err error }

func (e *ErrLectura) Error() string { return e.Err.Error() }
func (e *ErrLectura) Unwrap() error { return e.Err }

// Opciones de Recibir.
type Opciones struct {
	// SHA256 es el hash esperado, en hexadecimal, con el prefijo "sha256:"
	// de los digests OCI o sin él. Vacío: no se comprueba, solo se calcula
	// (Recibido.SHA256).
	SHA256 string
	// Size, si es > 0, es exactamente lo que tiene que medir.
	Size int64
	// Max, si es > 0, es el tope de lo que se lee (además de Size).
	Max int64
	// Perm son los permisos del fichero (0 = 0644). Se ponen con Chmod: la
	// umask no cuenta.
	Perm os.FileMode
	// Patron es el del nombre del temporal para CreateTemp ("" = "tmp.part-*").
	Patron string
	// Preparar, si no es nil, se llama con el temporal ya verificado y antes
	// de llevarlo al disco (un Chown, por ejemplo).
	Preparar func(*os.File) error
}

// Recibido es un temporal escrito, verificado y en disco, a falta de
// publicarlo (Publicar) o tirarlo (Descartar).
type Recibido struct {
	Ruta   string // el temporal
	N      int64  // bytes escritos
	SHA256 string // su sha256, en hexadecimal
}

// Recibir escribe r en un temporal nuevo de dir y lo verifica según o. Es la
// mitad de EscribirVerificado para quien tiene que decidir el rename por su
// cuenta (bajo un cerrojo, comparando con lo que ya había). Si falla, no
// queda temporal.
func Recibir(dir string, r io.Reader, o Opciones) (*Recibido, error) {
	patron := o.Patron
	if patron == "" {
		patron = "tmp.part-*"
	}
	perm := o.Perm
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.CreateTemp(dir, patron)
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	tope := int64(-1)
	switch {
	case o.Size > 0 && o.Max > 0:
		tope = min(o.Size, o.Max)
	case o.Size > 0:
		tope = o.Size
	case o.Max > 0:
		tope = o.Max
	}
	var src io.Reader = &lector{r: r}
	if tope >= 0 {
		// +1 para distinguir «justo el tope» de «se pasó» sin leer lo que sobra.
		src = io.LimitReader(src, tope+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		return nil, err
	}
	// Con tamaño declarado, lo que no mide eso es ErrTamaño aunque además
	// pase del tope: el tope es lo que se lee, el tamaño lo que se esperaba.
	switch {
	case o.Size > 0 && n != o.Size:
		return nil, &ErrTamaño{N: n, Quiero: o.Size}
	case o.Max > 0 && n > o.Max:
		return nil, &ErrDemasiado{Max: o.Max}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimPrefix(o.SHA256, "sha256:"); want != "" && got != want {
		return nil, &ErrSHA256{Got: got, Want: want}
	}
	if err := f.Chmod(perm); err != nil {
		return nil, err
	}
	if o.Preparar != nil {
		if err := o.Preparar(f); err != nil {
			return nil, err
		}
	}
	if err := Ordenar(f); err != nil {
		return nil, fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	ok = true
	return &Recibido{Ruta: tmp, N: n, SHA256: got}, nil
}

// Publicar mueve el temporal a dst (en el mismo directorio) y lleva el
// directorio al disco. Si falla, el temporal se borra.
func (t *Recibido) Publicar(dst string) error {
	if err := os.Rename(t.Ruta, dst); err != nil {
		os.Remove(t.Ruta)
		return err
	}
	// Como en Escribir: el rename ya ocurrió, no fallar aquí no esconde nada.
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Descartar borra el temporal.
func (t *Recibido) Descartar() { os.Remove(t.Ruta) }

// EscribirVerificado deja en dst lo que llega por r solo si tiene ese sha256
// (hexadecimal, con "sha256:" o sin él) y, si size > 0, ese tamaño. O dst
// queda con eso entero y en disco, o no se toca. El temporal es
// <base>.part-*, junto a dst.
func EscribirVerificado(dst string, r io.Reader, sha256 string, size int64) error {
	if strings.TrimPrefix(sha256, "sha256:") == "" {
		return errors.New("no sha256 to verify against")
	}
	t, err := Recibir(filepath.Dir(dst), r, Opciones{SHA256: sha256, Size: size,
		Patron: filepath.Base(dst) + ".part-*"})
	if err != nil {
		return err
	}
	return t.Publicar(dst)
}

// lector marca los fallos de r como ErrLectura.
type lector struct{ r io.Reader }

func (l *lector) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err != nil && err != io.EOF {
		err = &ErrLectura{Err: err}
	}
	return n, err
}
