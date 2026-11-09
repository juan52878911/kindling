//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"
)

// TestDiscoVariosBuferes: una petición con varios búferes de datos (que van
// en un solo preadv/pwritev) lee y escribe lo mismo que uno a uno, sin
// importar cómo se corte, y un búfer que no vale deja hechos los de antes y
// la petición en error, como antes. El disco de pruebas mide 1 MiB (2048
// sectores).
func TestDiscoVariosBuferes(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	escrito := make([]byte, 512+1536+512)
	r.Read(escrito)
	if e, u := esDisco(1, 10, []uint32{512, 1536, 512}, append([]byte(nil), escrito...), -1); e != 0 || u != 1 {
		t.Fatalf("OUT: status %d, used len %d; want 0, 1", e, u)
	}
	leido := make([]byte, len(escrito))
	if e, u := esDisco(0, 10, []uint32{1024, 1024, 512}, leido, -1); e != 0 || u != uint32(len(leido))+1 {
		t.Fatalf("IN: status %d, used len %d; want 0, %d", e, u, len(leido)+1)
	}
	if !bytes.Equal(leido, escrito) {
		t.Fatal("IN with other buffer sizes does not return what OUT wrote")
	}

	// El segundo búfer fuera de la RAM: el primero se lee igual.
	leido = make([]byte, 3*512)
	if e, u := esDisco(0, 10, []uint32{512, 512, 512}, leido, 1); e != 1 || u != 513 {
		t.Fatalf("IN with a bad buffer: status %d, used len %d; want 1, 513", e, u)
	}
	if !bytes.Equal(leido[:512], escrito[:512]) {
		t.Fatal("IN with a bad buffer did not read the buffers before it")
	}

	// Pasado el final del disco: el primer búfer (el último sector) se
	// escribe, el segundo no, y la petición falla.
	fin := make([]byte, 1024)
	r.Read(fin)
	if e, _ := esDisco(1, 2047, []uint32{512, 512}, append([]byte(nil), fin...), -1); e != 1 {
		t.Fatalf("OUT past the end: status %d, want 1", e)
	}
	ultimo := make([]byte, 512)
	if e, _ := esDisco(0, 2047, []uint32{512}, ultimo, -1); e != 0 || !bytes.Equal(ultimo, fin[:512]) {
		t.Fatalf("last sector: status %d, same data %v", e, bytes.Equal(ultimo, fin[:512]))
	}
	if e, _ := esDisco(0, 2048, []uint32{512}, make([]byte, 512), -1); e != 1 {
		t.Fatalf("IN at the end of the disk: status %d, want 1", e)
	}
	// Sin búferes de datos no hay nada que leer: bien, como antes.
	if e, u := esDisco(0, 0, nil, nil, -1); e != 0 || u != 1 {
		t.Fatalf("IN without data: status %d, used len %d; want 0, 1", e, u)
	}
}

// TestDiscoTandaAcotada: un invitado con la cola llena (512 WRITE del disco
// entero, 1016 KiB cada uno con búferes que aliasan la misma página) no tiene
// al hilo del disco en una sola tanda de medio GiB, que no dejaba entrar a
// khv_pause (espera a que acabe la tanda, con plazo). La tanda para en
// vblk_tanda_max bytes, para tras la petición en curso si la VM se está
// pausando, y una petición mayor que una tanda falla sin tocar el disco.
func TestDiscoTandaAcotada(t *testing.T) {
	const pet = 254 * 4096
	hechas, e := tandaDisco(false, 0)
	if hechas <= 0 || e != 0 {
		t.Fatalf("one batch: %d requests, status %d; want some and 0", hechas, e)
	}
	if max := int((64<<20)/pet) + 1; hechas > max {
		t.Fatalf("one batch answered %d whole-disk WRITEs (%d MiB); want at most %d", hechas, hechas*pet>>20, max)
	}
	if hechas, e = tandaDisco(true, 0); hechas != 1 || e != 0 {
		t.Fatalf("batch with the VM pausing: %d requests, status %d; want 1 and 0", hechas, e)
	}
	// Con un tope menor que una petición: cada una falla, y la tanda se corta
	// igual tras la primera.
	if hechas, e = tandaDisco(false, 512<<10); hechas != 1 || e != 1 {
		t.Fatalf("request larger than a batch: %d requests, status %d; want 1 and 1", hechas, e)
	}
}

// segmentos codifica segmentos de DISCARD/WRITE_ZEROES {sector, num, flags}.
func segmentos(s ...[3]uint64) []byte {
	b := make([]byte, 16*len(s))
	for i, x := range s {
		binary.LittleEndian.PutUint64(b[16*i:], x[0])
		binary.LittleEndian.PutUint32(b[16*i+8:], uint32(x[1]))
		binary.LittleEndian.PutUint32(b[16*i+12:], uint32(x[2]))
	}
	return b
}

// descartar manda un DISCARD (11) o WRITE_ZEROES (13) con esos segmentos en
// un solo búfer y devuelve el byte de estado.
func descartar(t *testing.T, tipo uint32, seg []byte) int {
	t.Helper()
	e, u := esDisco(tipo, 0, []uint32{uint32(len(seg))}, seg, -1)
	if u != 1 {
		t.Fatalf("type %d: used len %d, want 1 (only the status byte)", tipo, u)
	}
	return e
}

// llenar escribe datos al azar en los sectores [desde, desde+n) del disco de
// pruebas, en trozos de 64 KiB, y los devuelve.
func llenar(t *testing.T, r *rand.Rand, desde, n uint64) []byte {
	t.Helper()
	todo := make([]byte, n*512)
	r.Read(todo)
	for o := uint64(0); o < uint64(len(todo)); o += 0x10000 {
		f := min(o+0x10000, uint64(len(todo)))
		if e, _ := esDisco(1, desde+o/512, []uint32{uint32(f - o)}, append([]byte(nil), todo[o:f]...), -1); e != 0 {
			t.Fatalf("OUT at sector %d: status %d", desde+o/512, e)
		}
	}
	return todo
}

func leer(t *testing.T, sector, n uint64) []byte {
	t.Helper()
	b := make([]byte, n*512)
	for o := uint64(0); o < uint64(len(b)); o += 0x10000 {
		f := min(o+0x10000, uint64(len(b)))
		if e, _ := esDisco(0, sector+o/512, []uint32{uint32(f - o)}, b[o:f], -1); e != 0 {
			t.Fatalf("IN at sector %d: status %d", sector+o/512, e)
		}
	}
	return b
}

// TestDiscoDescarteYCeros: el disco de escritura anuncia DISCARD y
// WRITE_ZEROES con sus topes y la alineación del sistema de ficheros del
// host; un DISCARD sin alinear deja a cero lo descartado, intactos los
// sectores de al lado y devuelve al host los bloques enteros; WRITE_ZEROES
// con unmap también los devuelve y sin unmap los deja asignados. El disco de
// pruebas mide 1 MiB (2048 sectores).
func TestDiscoDescarteYCeros(t *testing.T) {
	defer modoDisco(false, sincFull)
	cfgDisco(0) // prepara el disco de pruebas
	modoDisco(false, sincFull)
	if f := cfgDisco(0x010); f&(1<<13) == 0 || f&(1<<14) == 0 {
		t.Fatalf("features %#x without DISCARD and WRITE_ZEROES", f)
	}
	maxD, segD, alin := cfgDisco(0x100+36), cfgDisco(0x100+40), cfgDisco(0x100+44)
	maxZ, segZ, unmap := cfgDisco(0x100+48), cfgDisco(0x100+52), cfgDisco(0x100+56)&0xff
	if maxD == 0 || segD == 0 || alin == 0 || maxZ == 0 || segZ != 1 || unmap != 1 {
		t.Fatalf("config: discard %d×%d align %d, write zeroes %d×%d unmap %d", maxD, segD, alin, maxZ, segZ, unmap)
	}
	r := rand.New(rand.NewSource(2))
	llenar(t, r, 0, 2048)
	antes := bloquesDisco()
	// [3, 1021): ni el principio ni el final caen en un bloque de 4 KiB.
	if e := descartar(t, 11, segmentos([3]uint64{3, 1018, 0})); e != 0 {
		t.Fatalf("DISCARD: status %d", e)
	}
	despues := bloquesDisco()
	t.Logf("st_blocks %d -> %d after a 509 KiB discard (alignment %d sectors)", antes, despues, alin)
	if antes-despues < 1018-2*int64(alin) {
		t.Fatalf("DISCARD returned %d blocks of 512 B, want at least %d", antes-despues, 1018-2*int64(alin))
	}
	d := leer(t, 0, 1024)
	if !bytes.Equal(d[3*512:1021*512], make([]byte, 1018*512)) {
		t.Fatal("discarded sectors do not read as zeros")
	}
	if bytes.Equal(d[:3*512], make([]byte, 3*512)) || bytes.Equal(d[1021*512:], make([]byte, 3*512)) {
		t.Fatal("DISCARD touched the sectors around it")
	}

	// WRITE_ZEROES con unmap: huecos, como el DISCARD.
	llenar(t, r, 1024, 1024)
	antes = bloquesDisco()
	if e := descartar(t, 13, segmentos([3]uint64{1024, 512, 1})); e != 0 {
		t.Fatalf("WRITE_ZEROES unmap: status %d", e)
	}
	if !bytes.Equal(leer(t, 1024, 512), make([]byte, 512*512)) {
		t.Fatal("WRITE_ZEROES unmap: does not read as zeros")
	}
	if b := bloquesDisco(); b >= antes {
		t.Fatalf("WRITE_ZEROES unmap: st_blocks %d -> %d, want fewer", antes, b)
	}
	// Sin unmap: ceros escritos, los bloques siguen ahí.
	antes = bloquesDisco()
	if e := descartar(t, 13, segmentos([3]uint64{1536, 511, 0})); e != 0 {
		t.Fatalf("WRITE_ZEROES: status %d", e)
	}
	if !bytes.Equal(leer(t, 1536, 511), make([]byte, 511*512)) {
		t.Fatal("WRITE_ZEROES: does not read as zeros")
	}
	if b := bloquesDisco(); b < antes {
		t.Fatalf("WRITE_ZEROES without unmap deallocated: st_blocks %d -> %d", antes, b)
	}
}

// TestDiscoDescarteValida: los segmentos se validan como los sectores de un
// read/write, uno malo deja el disco sin tocar, y un disco de solo lectura
// rechaza los dos tipos.
func TestDiscoDescarteValida(t *testing.T) {
	defer modoDisco(false, sincFull)
	cfgDisco(0)
	modoDisco(false, sincFull)
	r := rand.New(rand.NewSource(3))
	datos := llenar(t, r, 0, 64)
	casos := []struct {
		nombre string
		tipo   uint32
		seg    []byte
		want   int
	}{
		{"past the end", 11, segmentos([3]uint64{2040, 9, 0}), 1},
		{"sector past the end", 11, segmentos([3]uint64{2049, 0, 0}), 1},
		{"sector overflow", 11, segmentos([3]uint64{1<<64 - 1, 2, 0}), 1},
		{"sector*512 overflow", 13, segmentos([3]uint64{1 << 55, 1, 1}), 1},
		{"over the write zeroes limit", 13, segmentos([3]uint64{0, 1<<16 + 1, 1}), 1},
		{"unmap on a discard", 11, segmentos([3]uint64{0, 8, 1}), 2},
		{"unknown flag", 13, segmentos([3]uint64{0, 8, 2}), 2},
		{"two write zeroes segments", 13, segmentos([3]uint64{0, 8, 1}, [3]uint64{8, 8, 1}), 1},
		{"not a multiple of 16", 11, segmentos([3]uint64{0, 8, 0})[:15], 1},
		// El bueno primero: con el segundo malo no se descarta ninguno.
		{"good then bad flag", 11, segmentos([3]uint64{0, 8, 0}, [3]uint64{0, 8, 4}), 2},
		{"good then out", 11, segmentos([3]uint64{0, 8, 0}, [3]uint64{2047, 2, 0}), 1},
	}
	for _, c := range casos {
		if e := descartar(t, c.tipo, c.seg); e != c.want {
			t.Errorf("%s: status %d, want %d", c.nombre, e, c.want)
		}
	}
	if e, _ := esDisco(11, 0, nil, nil, -1); e != 1 {
		t.Errorf("no segments: status %d, want 1", e)
	}
	if e, _ := esDisco(11, 0, []uint32{16}, segmentos([3]uint64{0, 8, 0}), 0); e != 1 {
		t.Errorf("segments outside the RAM: status %d, want 1", e)
	}
	muchos := make([][3]uint64, 33)
	for i := range muchos {
		muchos[i] = [3]uint64{uint64(i) * 8, 8, 0}
	}
	if e := descartar(t, 11, segmentos(muchos...)); e != 1 {
		t.Errorf("33 segments: status %d, want 1", e)
	}
	if !bytes.Equal(leer(t, 0, 64), datos) {
		t.Fatal("a rejected request changed the disk")
	}
	// Hasta el último sector, y 32 segmentos, bien.
	if e := descartar(t, 11, segmentos([3]uint64{2040, 8, 0})); e != 0 {
		t.Errorf("up to the last sector: status %d, want 0", e)
	}
	if e := descartar(t, 11, segmentos(muchos[:32]...)); e != 0 {
		t.Errorf("32 segments: status %d, want 0", e)
	}

	// Un disco de solo lectura no los anuncia y los rechaza.
	datos = llenar(t, r, 0, 64)
	modoDisco(true, sincFull)
	if f := cfgDisco(0x010); f&(1<<13) != 0 || f&(1<<14) != 0 {
		t.Errorf("read-only disk offers DISCARD/WRITE_ZEROES (features %#x)", f)
	}
	for _, tipo := range []uint32{11, 13} {
		if e := descartar(t, tipo, segmentos([3]uint64{0, 8, 0})); e != 1 {
			t.Errorf("type %d on a read-only disk: status %d, want 1", tipo, e)
		}
	}
	modoDisco(false, sincFull)
	if !bytes.Equal(leer(t, 0, 64), datos) {
		t.Fatal("the read-only disk changed")
	}
}

// TestFlushModos: el FLUSH va bien en los tres modos; lo que cuesta cada uno
// en el host (escribir 4 KiB + FLUSH, 300 veces) queda en el log.
func TestFlushModos(t *testing.T) {
	defer modoDisco(false, sincFull)
	cfgDisco(0)
	buf := make([]byte, 4096)
	for _, m := range []struct {
		nombre string
		sinc   int
	}{{"full", sincFull}, {"barrier", sincBarrier}, {"none", sincNone}} {
		modoDisco(false, m.sinc)
		t0 := time.Now()
		for i := 0; i < 300; i++ {
			if e, _ := esDisco(1, uint64(i%256)*8, []uint32{4096}, buf, -1); e != 0 {
				t.Fatalf("%s: OUT status %d", m.nombre, e)
			}
			if e := flushDisco(); e != 0 {
				t.Fatalf("%s: FLUSH status %d", m.nombre, e)
			}
		}
		t.Logf("%s: 300 × (4 KiB + FLUSH) in %v on the host", m.nombre, time.Since(t0))
	}
}
