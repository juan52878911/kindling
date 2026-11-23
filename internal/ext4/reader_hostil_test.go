package ext4

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Imágenes ext4 hostiles: Read tiene que dar error (sin pánico) en menos de
// 1 s y sin pedir más de 64 MiB. Antes, un árbol de extents ancho o un
// directorio con i_size_high pedían GiB, y un inodo de 128 bytes tumbaba el
// proceso.

// imagenHostil escribe una imagen pequeña con un directorio, un fichero y un
// enlace lento (más de 60 bytes de destino: va en un bloque) y la devuelve.
func imagenHostil(t *testing.T) []byte {
	t.Helper()
	tm := time.Unix(1, 0)
	root := NewDir(0o755, 0, 0, tm)
	d := NewDir(0o755, 0, 0, tm)
	root.SetChild("d", d)
	d.SetChild("f", &Node{Mode: ModeReg | 0o644, Size: 5, Data: Bytes("hola\n")})
	d.SetChild("l", &Node{Mode: ModeLink | 0o777, Target: strings.Repeat("x", 100)})
	p := t.TempDir() + "/sana.ext4"
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Write(f, root, nil, Options{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var le = binary.LittleEndian

// inodoOff da dónde empieza el inodo ino (del grupo 0) en la imagen.
func inodoOff(b []byte, ino int) int {
	isz := int(le.Uint16(b[1024+88:]))
	first := int(le.Uint32(b[1024+20:]))
	it := int(le.Uint32(b[(first+1)*4096+8:]))
	return it*4096 + (ino-1)*isz
}

// inodoDe busca en el grupo 0 el primer inodo con ese tipo (ModeReg...).
func inodoDe(t *testing.T, b []byte, tipo uint32) int {
	t.Helper()
	ipg := int(le.Uint32(b[1024+40:]))
	for ino := 3; ino <= ipg; ino++ {
		if uint32(le.Uint16(b[inodoOff(b, ino):]))&ModeType == tipo {
			return ino
		}
	}
	t.Fatalf("no hay inodo de tipo %#o", tipo)
	return 0
}

// leerAcotado lee la imagen con Read vigilando el tiempo y la memoria. Si
// pasa de 5 s o de 1 GiB de heap, para el proceso: sin el arreglo, seguir
// esperando era llenar la memoria del anfitrión.
func leerAcotado(t *testing.T, b []byte) error {
	t.Helper()
	p := t.TempDir() + "/h.ext4"
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	type res struct {
		err error
		pan any
	}
	done := make(chan res, 1)
	t0 := time.Now()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- res{pan: p}
			}
		}()
		_, err := Read(f)
		done <- res{err: err}
	}()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case r := <-done:
			dt := time.Since(t0)
			var m1 runtime.MemStats
			runtime.ReadMemStats(&m1)
			alloc := (m1.TotalAlloc - m0.TotalAlloc) >> 20
			t.Logf("%v en %v, %d MiB pedidos", r.err, dt, alloc)
			if r.pan != nil {
				t.Fatalf("pánico: %v", r.pan)
			}
			if dt > time.Second {
				t.Errorf("tardó %v (tope 1 s)", dt)
			}
			if alloc >= 64 {
				t.Errorf("pidió %d MiB (tope 64)", alloc)
			}
			return r.err
		case <-tick.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if time.Since(t0) > 5*time.Second || m.HeapAlloc > 1<<30 {
				panic(fmt.Sprintf("%s: Read sigue tras %v con %d MiB de heap", t.Name(), time.Since(t0), m.HeapAlloc>>20))
			}
		}
	}
}

func TestReadImagenSana(t *testing.T) {
	if err := leerAcotado(t, imagenHostil(t)); err != nil {
		t.Fatal(err)
	}
}

// s_inode_size = 128 es válido en ext4, pero aquí los campos extra se leían
// igual, fuera del inodo: pánico.
func TestReadInodo128(t *testing.T) {
	b := imagenHostil(t)
	le.PutUint16(b[1024+88:], 128)
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}

// Árbol de extents de 5 niveles de 340 índices cada uno, todos al mismo
// bloque: 340^4 nodos que leer sobre una imagen de unos cientos de KiB.
func TestReadExtentsAnchos(t *testing.T) {
	b := imagenHostil(t)
	off := inodoOff(b, 2)
	ib := b[off+40 : off+100]
	last := uint32(len(b)/4096 - 1)
	nodo := func(dst []byte, n int, depth uint16, hijo uint32) {
		clear(dst)
		le.PutUint16(dst[0:], 0xF30A)
		le.PutUint16(dst[2:], uint16(n))
		le.PutUint16(dst[4:], uint16(n))
		le.PutUint16(dst[6:], depth)
		for i := 0; i < n; i++ {
			e := dst[12+12*i:]
			if depth == 0 {
				le.PutUint16(e[4:], 1)
				le.PutUint32(e[8:], last)
			} else {
				le.PutUint32(e[4:], hijo)
			}
		}
	}
	blq := func(k uint32) []byte { return b[int(last-k)*4096 : int(last-k)*4096+4096] }
	nodo(ib, 4, 5, last-1)
	for k := uint32(1); k <= 4; k++ {
		nodo(blq(k), 340, uint16(5-k), last-k-1)
	}
	nodo(blq(5), 340, 0, 0)
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}

// Un directorio que dice medir 64 TiB (i_size_high = 16): se leía entero, con
// ceros de los huecos, a memoria.
func TestReadDirectorioEnorme(t *testing.T) {
	b := imagenHostil(t)
	le.PutUint32(b[inodoOff(b, 2)+108:], 16)
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}

// Un enlace simbólico lento de 1 TiB: lo mismo que el directorio.
func TestReadEnlaceEnorme(t *testing.T) {
	b := imagenHostil(t)
	le.PutUint32(b[inodoOff(b, inodoDe(t, b, ModeLink))+108:], 256)
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}

// Un fichero de tamaño negativo (i_size_high con el bit alto).
func TestReadTamañoNegativo(t *testing.T) {
	b := imagenHostil(t)
	le.PutUint32(b[inodoOff(b, inodoDe(t, b, ModeReg))+108:], 0x80000000)
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}

// Un superbloque que declara 2^40 bloques en una imagen de KiB: la tabla de
// descriptores de grupo se pedía entera (GiB) antes de leerla.
func TestReadBloquesDeMas(t *testing.T) {
	b := imagenHostil(t)
	sb := b[1024:]
	le.PutUint32(sb[96:], le.Uint32(sb[96:])|incompat64bit)
	le.PutUint16(sb[254:], 64)
	le.PutUint32(sb[4:], 0)
	le.PutUint32(sb[336:], 1<<8) // 2^40 bloques
	le.PutUint32(sb[20:], 0)     // primer bloque de datos
	le.PutUint32(sb[32:], 32768) // 2^25 grupos
	le.PutUint32(sb[40:], 64)
	le.PutUint32(sb[0:], 1<<31) // 2^25 grupos × 64 inodos
	if leerAcotado(t, b) == nil {
		t.Fatal("aceptada")
	}
}
