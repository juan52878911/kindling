package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// BarrerTemporales borra lo que dejaron las construcciones y descargas que se
// cortaron con el daemon (murió, se apagó el Mac): sus directorios de trabajo
// y ficheros de build/ y los temporales de las cachés de blobs. Cada uno solo
// se borraba en el defer de quien lo creó, que ya no va a correr, así que se
// acumulaban sin que nadie los mirase.
//
// Es para el arranque, antes de Listen: con el cerrojo de la raíz tomado
// (New) y sin ninguna petición atendida, nada de esto puede estar en uso.
func (s *Server) BarrerTemporales() { barrerTemporales(s.root) }

// barrerTemporales barre root y devuelve cuántas entradas borró.
func barrerTemporales(root string) int {
	n := 0
	borrar := func(p string) {
		if err := os.RemoveAll(p); err != nil {
			log.Printf("startup sweep: %v", err)
			return
		}
		n++
	}

	// build/: los <name>.NNNN de prepararTrabajo (MkdirTemp) y los
	// imagefile-* de las subidas en línea. Solo esos: lo que no se reconoce
	// se deja.
	build := filepath.Join(root, "build")
	if es, err := os.ReadDir(build); err == nil {
		for _, e := range es {
			if trabajoDeConstruccion(e) || (!e.IsDir() && strings.HasPrefix(e.Name(), "imagefile-")) {
				borrar(filepath.Join(build, e.Name()))
			}
		}
	}

	// La caché de blobs de root: los .part de las descargas (<hex>.part de
	// antes, <hex>.part-NNNN de durable.Recibir) y de las subidas
	// (<hex>.<rnd>.part de antes).
	barrerFicheros(dirCacheOCI(root), func(nombre string) bool {
		return strings.HasSuffix(nombre, ".part") || strings.Contains(nombre, ".part-")
	}, borrar)

	// Las verificadas (la común y la de cada origen): los .tmp-* de
	// copiarVerificado.
	verificadas := filepath.Join(root, "cache", "verified")
	dirs := []string{filepath.Join(verificadas, "oci", "sha256")}
	if es, err := os.ReadDir(verificadas); err == nil {
		for _, e := range es {
			if e.IsDir() && e.Name() != "oci" {
				dirs = append(dirs, filepath.Join(verificadas, e.Name(), "oci", "sha256"))
			}
		}
	}
	for _, d := range dirs {
		barrerFicheros(d, func(nombre string) bool { return strings.HasPrefix(nombre, ".tmp-") }, borrar)
	}

	if n > 0 {
		log.Printf("startup sweep: removed %d leftover build directories and temporaries", n)
	}
	return n
}

// barrerFicheros borra los ficheros regulares de dir que cumplen es.
func barrerFicheros(dir string, es func(string) bool, borrar func(string)) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Type().IsRegular() && es(e.Name()) {
			borrar(filepath.Join(dir, e.Name()))
		}
	}
}

// trabajoDeConstruccion dice si e es un directorio de trabajo de
// prepararTrabajo: <name>.<dígitos> (MkdirTemp con el patrón name+".").
func trabajoDeConstruccion(e os.DirEntry) bool {
	if !e.IsDir() {
		return false
	}
	i := strings.LastIndexByte(e.Name(), '.')
	if i <= 0 || i == len(e.Name())-1 {
		return false
	}
	for _, c := range e.Name()[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
