// Package durable escribe ficheros de estado que tienen que sobrevivir a un corte.
package durable

import (
	"fmt"
	"os"
	"path/filepath"
)

// Escribir deja `datos` en `ruta` de forma que, tras un corte de luz, en disco
// quede el contenido VIEJO completo o el NUEVO completo, nunca una mezcla ni un
// fichero a medias.
//
// Son cuatro pasos y los cuatro hacen falta:
//
//  1. escribir a un temporal al lado — en el MISMO directorio, porque el rename
//     del paso 3 solo es atomico dentro del mismo sistema de ficheros;
//  2. fsync del TEMPORAL, o el rename acabaria apuntando a un fichero cuyo
//     contenido todavia vive en la cache y se pierde con el corte;
//  3. rename, lo unico atomico que ofrece POSIX;
//  4. fsync del DIRECTORIO, o el propio rename puede no haber llegado al disco.
//
// Saltarse el 4 es el error clasico, y el peor: al arrancar se lee el fichero
// VIEJO y se da por bueno. No hay error, no hay hueco, no hay nada que avise —
// simplemente el sistema recuerda algo que ya no es verdad.
func Escribir(ruta string, datos []byte, perm os.FileMode) error {
	tmp := ruta + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// Cualquier fallo a partir de aqui borra el temporal: dejarlo tirado
	// convertiria un fallo de escritura en basura permanente al lado del bueno.
	if _, err := f.Write(datos); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, ruta); err != nil {
		os.Remove(tmp)
		return err
	}
	// El fsync del directorio no devuelve error a proposito: si el directorio no
	// se puede abrir, el rename YA ocurrio y el dato esta. Fallar aqui convertiria
	// un exito en un error, y el llamador reescribiria sin necesidad.
	if d, err := os.Open(filepath.Dir(ruta)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Renombrar mueve `tmp` a `destino` de forma durable: fsync del FICHERO
// primero (por lo mismo que en Escribir: sin él el rename acaba apuntando a
// contenido que todavia vive en la cache), rename, y fsync del DIRECTORIO
// despues (o el rename mismo puede no haber llegado al disco).
//
// A diferencia de Escribir, aqui el contenido ya esta en disco de antemano
// —tipicamente un fichero grande construido por otro proceso (mkfs.ext4,
// dd)— y lo unico que falta es publicarlo de forma que EXISTIR EN destino
// IMPLIQUE ESTAR COMPLETO. `tmp` y `destino` deben estar en el MISMO
// directorio: el rename solo es atomico dentro del mismo sistema de ficheros.
func Renombrar(tmp, destino string) error {
	f, err := os.Open(tmp)
	if err != nil {
		return fmt.Errorf("opening %s to fsync before rename: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, destino); err != nil {
		return err
	}
	// Ver el comentario equivalente en Escribir: el rename ya ocurrio, asi que
	// no fallar aqui no esconde nada.
	if d, err := os.Open(filepath.Dir(destino)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
