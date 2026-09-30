// Package esquema versiona los ficheros de estado que kindling deja en disco.
//
// Cada fichero persistido lleva un campo "schema" con un entero que sube cuando
// su formato cambia de forma que un lector viejo lo entendería mal. Las reglas
// son tres y las mismas para todos (ver docs/actualizar.md):
//
//  1. Un fichero SIN el campo es la versión 0: lo que había antes de versionar.
//     Se lee y se migra hacia delante.
//  2. Antes de la primera escritura que cambia la versión, se guarda una copia
//     del original al lado (<ruta>.v<N>.bak). Migrar es irreversible; la copia
//     es lo que permite volver al binario anterior.
//  3. Un fichero de una versión MAYOR que la que entiende el binario no se lee,
//     no se aparta y, sobre todo, no se escribe encima: lo dejó un kling más
//     nuevo, y un binario viejo que lo "arreglara" perdería lo que no conoce.
//
// No hay aquí un marco de migraciones: cada dueño de un fichero sabe pasar de
// su versión N a la N+1. Esto solo es lo común —leer la versión, reconocer el
// fichero del futuro y guardar la copia— para que no se escriba cinco veces
// ligeramente distinto.
package esquema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Cabecera se incrusta en el struct de un fichero versionado para que el campo
// se llame igual en todos.
type Cabecera struct {
	Schema int `json:"schema"`
}

// ErrMasNuevo es el error de un fichero escrito por un kling más nuevo.
type ErrMasNuevo struct {
	Ruta      string
	Leida     int // la versión del fichero
	Soportada int // la mayor que entiende este binario
}

func (e *ErrMasNuevo) Error() string {
	return fmt.Sprintf("%s has schema %d but this kling only understands up to %d: "+
		"it was written by a newer version. Nothing was changed; run that version again "+
		"(or restore %s.v%d.bak if it exists)", e.Ruta, e.Leida, e.Soportada, e.Ruta, e.Soportada)
}

// EsMasNuevo dice si err viene de un fichero del futuro.
func EsMasNuevo(err error) bool {
	var e *ErrMasNuevo
	return errors.As(err, &e)
}

// Version lee el campo "schema" de un fichero JSON sin interpretar el resto.
//
// Un documento que no es un objeto (el state.json de antes era un array) o un
// objeto sin el campo es la versión 0. Un "schema" que no es un entero no
// negativo es un error: no se puede saber qué formato es, y adivinarlo es
// justo lo que no hay que hacer.
func Version(b []byte) (int, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return 0, nil
	}
	var c struct {
		Schema *json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, err
	}
	if c.Schema == nil {
		return 0, nil
	}
	var v int
	if err := json.Unmarshal(*c.Schema, &v); err != nil || v < 0 {
		return 0, fmt.Errorf("invalid schema field %s", *c.Schema)
	}
	return v, nil
}

// Comprobar lee la versión de b y devuelve *ErrMasNuevo si es mayor que
// soportada. ruta solo sirve para el mensaje.
func Comprobar(ruta string, b []byte, soportada int) (int, error) {
	v, err := Version(b)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ruta, err)
	}
	if v > soportada {
		return v, &ErrMasNuevo{Ruta: ruta, Leida: v, Soportada: soportada}
	}
	return v, nil
}

// RutaRespaldo es donde Respaldar deja la copia de la versión v de ruta.
func RutaRespaldo(ruta string, v int) string { return fmt.Sprintf("%s.v%d.bak", ruta, v) }

// Respaldar copia ruta a <ruta>.v<v>.bak antes de migrarla. Si la copia ya
// existe no la toca: la buena es la primera, la del fichero tal como lo dejó
// la versión anterior, y un segundo intento de migración no debe pisarla con
// algo a medio migrar. Que ruta no exista no es un error (nada que guardar).
func Respaldar(ruta string, v int) error {
	dst := RutaRespaldo(ruta, v)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	src, err := os.Open(ruta)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
