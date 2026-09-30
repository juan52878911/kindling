// Package verity calcula en Go el árbol de hashes de dm-verity y su FEC
// (Reed-Solomon), con el mismo formato que `veritysetup format
// --no-superblock` de cryptsetup: bloques de 4 KiB, sha256, versión 1 (la sal
// va delante del bloque), los niveles del árbol de arriba abajo y el FEC
// RS(255, 255-raíces) intercalado como lo lee el núcleo
// (drivers/md/dm-verity-fec.c).
//
// Append deja el árbol y el FEC pegados detrás de los datos en el mismo
// fichero, como image/verity.sh del prototipo Android: la imagen se engancha
// como un solo disco y el ext4 ignora lo que hay tras su último bloque.
package verity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
)

// BlockSize es el tamaño de bloque de datos y de hashes.
const BlockSize = 4096

const hashesPerBlock = BlockSize / sha256.Size // 128

// Result describe lo que escribió Append.
type Result struct {
	RootHash   []byte
	Salt       []byte
	DataBlocks uint64
	HashBlocks uint64
	FECRoots   int
	FECBlocks  uint64 // bloques del área de FEC (0 sin FEC)
}

// HashStart es el primer bloque del árbol (justo detrás de los datos).
func (r Result) HashStart() uint64 { return r.DataBlocks }

// FECStart es el primer bloque del FEC (justo detrás del árbol).
func (r Result) FECStart() uint64 { return r.DataBlocks + r.HashBlocks }

// TotalBlocks es el tamaño del fichero entero, en bloques.
func (r Result) TotalBlocks() uint64 { return r.FECStart() + r.FECBlocks }

// Table es la tabla de device-mapper con dev como dispositivo de datos, de
// hashes y de FEC (la de verity.sh, con @DEV@ en su lugar).
func (r Result) Table(dev string) string {
	t := fmt.Sprintf("0 %d verity 1 %s %s %d %d %d %d sha256 %s %s",
		r.DataBlocks*BlockSize/512, dev, dev, BlockSize, BlockSize, r.DataBlocks, r.HashStart(),
		hex.EncodeToString(r.RootHash), saltHex(r.Salt))
	if r.FECRoots > 0 {
		t += fmt.Sprintf(" 8 use_fec_from_device %s fec_roots %d fec_blocks %d fec_start %d",
			dev, r.FECRoots, r.FECStart(), r.FECStart())
	}
	return t
}

func saltHex(s []byte) string {
	if len(s) == 0 {
		return "-"
	}
	return hex.EncodeToString(s)
}

// levels da, de abajo arriba, cuántos bloques tiene cada nivel del árbol.
func levels(dataBlocks uint64) []uint64 {
	var out []uint64
	n := dataBlocks
	for {
		n = (n + hashesPerBlock - 1) / hashesPerBlock
		out = append(out, n)
		if n <= 1 {
			return out
		}
	}
}

// HashBlocks es el tamaño del árbol para dataBlocks bloques de datos.
func HashBlocks(dataBlocks uint64) uint64 {
	var t uint64
	for _, n := range levels(dataBlocks) {
		t += n
	}
	return t
}

// FECBlocks es el tamaño del área de FEC que cubre blocks bloques.
func FECBlocks(blocks uint64, roots int) uint64 {
	if roots == 0 {
		return 0
	}
	rsn := uint64(255 - roots)
	rounds := (blocks + rsn - 1) / rsn
	return rounds * uint64(roots)
}

// Append calcula el árbol de los primeros dataBlocks bloques de f y lo
// escribe detrás, seguido del FEC con fecRoots raíces (0 = sin FEC; el núcleo
// admite de 2 a 24). Trunca f al tamaño final.
func Append(f *os.File, dataBlocks uint64, salt []byte, fecRoots int) (Result, error) {
	if dataBlocks == 0 {
		return Result{}, errors.New("no data blocks")
	}
	if fecRoots != 0 && (fecRoots < 2 || fecRoots > 24) {
		return Result{}, fmt.Errorf("fec roots must be 0 or 2..24, not %d", fecRoots)
	}
	res := Result{Salt: salt, DataBlocks: dataBlocks, HashBlocks: HashBlocks(dataBlocks), FECRoots: fecRoots}
	res.FECBlocks = FECBlocks(res.FECStart(), fecRoots)
	if err := f.Truncate(int64(res.FECStart()) * BlockSize); err != nil {
		return res, err
	}
	root, err := writeTree(f, dataBlocks, salt)
	if err != nil {
		return res, err
	}
	res.RootHash = root
	if fecRoots > 0 {
		if err := writeFEC(f, res.FECStart(), fecRoots, int64(res.FECStart())*BlockSize); err != nil {
			return res, err
		}
	}
	if err := f.Truncate(int64(res.TotalBlocks()) * BlockSize); err != nil {
		return res, err
	}
	return res, nil
}

// hashBlocks calcula sha256(sal || bloque) de count bloques de r desde el
// bloque first, en paralelo, y deja los hashes seguidos en out.
func hashBlocks(r io.ReaderAt, first, count uint64, salt []byte, out []byte) error {
	workers := runtime.GOMAXPROCS(0)
	const chunk = 1024 // bloques por tarea
	type job struct{ start, n uint64 }
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, chunk*BlockSize)
			h := sha256.New()
			for j := range jobs {
				b := buf[:j.n*BlockSize]
				if _, err := r.ReadAt(b, int64(first+j.start)*BlockSize); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				for i := uint64(0); i < j.n; i++ {
					h.Reset()
					h.Write(salt)
					h.Write(b[i*BlockSize : (i+1)*BlockSize])
					h.Sum(out[(j.start+i)*sha256.Size : (j.start+i)*sha256.Size])
				}
			}
		}()
	}
	for s := uint64(0); s < count; s += chunk {
		n := uint64(chunk)
		if s+n > count {
			n = count - s
		}
		jobs <- job{s, n}
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

// writeTree escribe los niveles del árbol detrás de los datos (el de más
// arriba primero, como el núcleo espera) y devuelve la raíz.
func writeTree(f *os.File, dataBlocks uint64, salt []byte) ([]byte, error) {
	lv := levels(dataBlocks)
	// Posición de cada nivel: del de arriba al de abajo.
	pos := make([]uint64, len(lv))
	p := dataBlocks
	for i := len(lv) - 1; i >= 0; i-- {
		pos[i] = p
		p += lv[i]
	}
	// Nivel 0: hashes de los datos.
	hashes := make([]byte, lv[0]*BlockSize)
	if err := hashBlocks(f, 0, dataBlocks, salt, hashes); err != nil {
		return nil, err
	}
	for i := range lv {
		if _, err := f.WriteAt(hashes, int64(pos[i])*BlockSize); err != nil {
			return nil, err
		}
		if i == len(lv)-1 {
			break
		}
		next := make([]byte, lv[i+1]*BlockSize)
		if err := hashBlocks(bytes.NewReader(hashes), 0, lv[i], salt, next); err != nil {
			return nil, err
		}
		hashes = next
	}
	h := sha256.New()
	h.Write(salt)
	h.Write(hashes[:BlockSize])
	return h.Sum(nil), nil
}

// Verify recalcula el árbol de f y lo compara con el que tiene escrito y
// con res.RootHash; con FEC, también la paridad.
func Verify(f *os.File, res Result) error {
	lv := levels(res.DataBlocks)
	hashes := make([]byte, lv[0]*BlockSize)
	if err := hashBlocks(f, 0, res.DataBlocks, res.Salt, hashes); err != nil {
		return err
	}
	p := res.DataBlocks
	pos := make([]uint64, len(lv))
	for i := len(lv) - 1; i >= 0; i-- {
		pos[i] = p
		p += lv[i]
	}
	for i := range lv {
		disk := make([]byte, len(hashes))
		if _, err := f.ReadAt(disk, int64(pos[i])*BlockSize); err != nil {
			return err
		}
		if !bytes.Equal(disk, hashes) {
			return fmt.Errorf("hash tree level %d does not match the data", i)
		}
		if i == len(lv)-1 {
			break
		}
		next := make([]byte, lv[i+1]*BlockSize)
		if err := hashBlocks(bytes.NewReader(hashes), 0, lv[i], res.Salt, next); err != nil {
			return err
		}
		hashes = next
	}
	h := sha256.New()
	h.Write(res.Salt)
	h.Write(hashes[:BlockSize])
	if !bytes.Equal(h.Sum(nil), res.RootHash) {
		return errors.New("root hash does not match")
	}
	if res.FECRoots > 0 {
		tmp, err := os.CreateTemp("", "verity-fec-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if err := fecTo(f, res.FECStart(), res.FECRoots, tmp, 0); err != nil {
			return err
		}
		want := make([]byte, res.FECBlocks*BlockSize)
		got := make([]byte, len(want))
		if _, err := tmp.ReadAt(want, 0); err != nil {
			return err
		}
		if _, err := f.ReadAt(got, int64(res.FECStart())*BlockSize); err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return errors.New("FEC parity does not match")
		}
	}
	return nil
}
