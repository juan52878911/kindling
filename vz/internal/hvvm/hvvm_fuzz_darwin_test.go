//go:build darwin && arm64

package hvvm

import (
	"encoding/binary"
	"testing"
)

// cola arma una entrada para probarColas: el dispositivo dev y sus colas de
// num descriptores con la tabla, el anillo avail y el used en esos
// desplazamientos de la RAM, que va detrás (desde RAM_BASE).
func cola(dev byte, num uint16, desc, avail, used uint32, ram []byte) []byte {
	b := make([]byte, 64+len(ram))
	b[0] = dev
	for q := 0; q < 3; q++ {
		c := b[2+q*14:]
		binary.LittleEndian.PutUint16(c, num)
		// prueba.c resta 0x1000 a cada dirección: se compensa.
		binary.LittleEndian.PutUint32(c[2:], desc+0x1000)
		binary.LittleEndian.PutUint32(c[6:], avail+0x1000)
		binary.LittleEndian.PutUint32(c[10:], used+0x1000)
	}
	copy(b[64:], ram)
	return b
}

// ramCon pone los descriptores (addr, len, flags, next) desde 0 y un avail
// con esos heads en 0x1000; el used queda en 0x2000.
func ramCon(descs [][4]uint64, heads ...uint16) []byte {
	ram := make([]byte, 0x4000)
	for i, d := range descs {
		o := i * 16
		binary.LittleEndian.PutUint64(ram[o:], d[0])
		binary.LittleEndian.PutUint32(ram[o+8:], uint32(d[1]))
		binary.LittleEndian.PutUint16(ram[o+12:], uint16(d[2]))
		binary.LittleEndian.PutUint16(ram[o+14:], uint16(d[3]))
	}
	binary.LittleEndian.PutUint16(ram[0x1002:], uint16(len(heads)))
	for i, h := range heads {
		binary.LittleEndian.PutUint16(ram[0x1004+2*i:], h)
	}
	return ram
}

const ramBase = 0x80000000

// semillas son los casos que rompían o podían romper: cadenas en ciclo,
// indirectas, más largas que la cola, longitudes enormes, direcciones fuera
// de la RAM, un disco sin byte de estado y una escritura fuera del disco.
func semillas() [][]byte {
	var s [][]byte
	ciclo := ramCon([][4]uint64{{ramBase + 0x3000, 64, 1, 1}, {ramBase + 0x3000, 64, 1, 0}}, 0)
	indirecta := ramCon([][4]uint64{{ramBase + 0x3000, 64, 4, 0}}, 0)
	enorme := ramCon([][4]uint64{{ramBase + 0x3000, 0xffffffff, 2, 0}}, 0)
	fuera := ramCon([][4]uint64{{0x10, 64, 3, 1}, {ramBase + 0x100000 - 8, 64, 2, 0}}, 0, 7, 0xffff)
	blkFuera := ramCon([][4]uint64{{ramBase + 0x3000, 16, 1, 1}, {ramBase + 0x3100, 512, 1, 2}, {ramBase + 0x3200, 1, 2, 0}}, 0)
	binary.LittleEndian.PutUint32(blkFuera[0x3000:], 1) // OUT
	binary.LittleEndian.PutUint64(blkFuera[0x3008:], 1<<40)
	sinEstado := ramCon([][4]uint64{{ramBase + 0x3000, 16, 0, 0}}, 0)
	for dev := byte(0); dev < 5; dev++ {
		for _, num := range []uint16{0, 1, 2, 256, 257, 0xffff} {
			for _, ram := range [][]byte{ciclo, indirecta, enorme, fuera, blkFuera, sinEstado} {
				s = append(s, cola(dev, num, 0, 0x1000, 0x2000, ram))
			}
		}
		// Anillos encima de la tabla, y en el borde de la RAM.
		s = append(s, cola(dev, 4, 0, 0, 0, ciclo))
		s = append(s, cola(dev, 256, 0x100000-16, 0x100000-6, 0x100000-6, ciclo))
	}
	return s
}

// Las semillas corren siempre, también sin -fuzz.
func TestColasHostiles(t *testing.T) {
	for _, s := range semillas() {
		probarColas(s)
	}
}

// go test -fuzz FuzzColas ./internal/hvvm: cualquier entrada es un invitado
// que programa un dispositivo y le hace notify; no debe romper el proceso.
func FuzzColas(f *testing.F) {
	for _, s := range semillas() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		probarColas(data)
	})
}

func TestVqCadena(t *testing.T) {
	casos := []struct {
		descs [][4]uint64
		head  uint16
		num   uint16
		max   int
		want  int
	}{
		{[][4]uint64{{0, 1, 1, 1}, {0, 1, 1, 2}, {0, 1, 0, 0}}, 0, 4, 16, 3}, // válida
		{[][4]uint64{{0, 1, 1, 1}, {0, 1, 1, 0}}, 0, 2, 16, -1},              // ciclo
		{[][4]uint64{{0, 1, 4, 0}}, 0, 2, 16, -1},                            // indirecta
		{[][4]uint64{{0, 1, 1, 9}}, 0, 2, 16, -1},                            // next fuera de la tabla
		{[][4]uint64{{0, 1, 0, 0}}, 5, 2, 16, -1},                            // head fuera
		{[][4]uint64{{0, 1, 1, 1}, {0, 1, 1, 2}, {0, 1, 0, 0}}, 0, 4, 2, -1}, // más larga que el tope
	}
	for i, c := range casos {
		if got := cadena(c.descs, c.head, c.num, c.max); got != c.want {
			t.Errorf("case %d: vq_cadena = %d, want %d", i, got, c.want)
		}
	}
}
