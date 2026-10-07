package digest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
)

// TREE: el digest del overlay de un dorado. sha256 de un fichero es
// secuencial (cada bloque depende del anterior), así que no se reparte entre
// núcleos, y los ceros de un hueco se hashean igual que los datos: el overlay
// son 512 MiB nominales y se leían enteros en cada commit (1,3 s de 2,2 en el
// lab, una CPU sin SHA-NI).
//
// Tree parte el fichero en trozos de 4 MiB, hace el sha256 de cada uno en
// paralelo y el sha256 de la lista. Un trozo que el sistema de ficheros dice
// que es todo hueco (SEEK_DATA) no se lee: su hash es el de 4 MiB de ceros,
// que es lo que se leería. Sigue siendo sha256 de punta a punta; cambia la
// forma, y por eso lleva el prefijo y va en otro campo del meta
// (api.Snapshot.RootfsDigest), no en el de un sha256 plano.

// TreePrefix va delante del hex: dice el algoritmo, para poder cambiarlo.
const TreePrefix = "sha256-tree-4m:"

const treeChunk = 4 << 20

var zeroChunk = sync.OnceValue(func() [32]byte {
	return sha256.Sum256(make([]byte, treeChunk))
})

// Tree devuelve el digest en árbol de path, con TreePrefix.
func Tree(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	n := int((size + treeChunk - 1) / treeChunk)
	conDatos := chunksConDatos(f, size, n)

	sums := make([][32]byte, n)
	work := make(chan int)
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for w := 0; w < min(runtime.GOMAXPROCS(0), 8, max(n, 1)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			h := sha256.New()
			for i := range work {
				off := int64(i) * treeChunk
				l := min(int64(treeChunk), size-off)
				if conDatos != nil && !conDatos[i] && l == treeChunk {
					sums[i] = zeroChunk()
					continue
				}
				h.Reset()
				if _, err := io.CopyBuffer(h, io.NewSectionReader(f, off, l), buf); err != nil {
					select {
					case errs <- err:
					default:
					}
					continue
				}
				h.Sum(sums[i][:0])
			}
		}()
	}
	for i := 0; i < n; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	select {
	case err := <-errs:
		return "", err
	default:
	}

	top := sha256.New()
	top.Write([]byte("kindling " + TreePrefix + "\n"))
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(size))
	top.Write(sz[:])
	for i := range sums {
		top.Write(sums[i][:])
	}
	return TreePrefix + hex.EncodeToString(top.Sum(nil)), nil
}

// chunksConDatos dice qué trozos tienen algo que no es hueco, o nil si el
// sistema de ficheros no lo sabe decir (entonces se leen todos).
func chunksConDatos(f *os.File, size int64, n int) []bool {
	if seekData < 0 {
		return nil
	}
	out := make([]bool, n)
	for off := int64(0); off < size; {
		d, err := f.Seek(off, seekData)
		if err != nil {
			if esFinDeDatos(err) {
				break // de off al final, todo hueco
			}
			return nil
		}
		h, err := f.Seek(d, seekHole)
		if err != nil || h <= d {
			return nil
		}
		h = min(h, size)
		for i := d / treeChunk; i <= (h-1)/treeChunk && i < int64(n); i++ {
			out[i] = true
		}
		off = h
	}
	return out
}

// Matches dice si path tiene el digest want, sea un sha256 plano en hex (los
// dorados de antes) o uno en árbol (con TreePrefix).
func Matches(path, want string) (got string, ok bool, err error) {
	if strings.HasPrefix(want, TreePrefix) {
		got, err = Tree(path)
	} else if strings.Contains(want, ":") {
		return "", false, fmt.Errorf("unknown digest %q", want[:strings.Index(want, ":")])
	} else {
		got, err = File(path)
	}
	if err != nil {
		return "", false, err
	}
	return got, got == want, nil
}
