//go:build darwin && arm64

package hvvm

import (
	"encoding/binary"
	"syscall"
	"testing"
	"time"
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
	// DISCARD y WRITE_ZEROES: dos segmentos (uno bueno y uno que se sale del
	// disco o lleva flags), en dos búferes, el segundo de 17 B.
	descarte := func(tipo uint32, flags uint32) []byte {
		r := ramCon([][4]uint64{{ramBase + 0x3000, 16, 1, 1}, {ramBase + 0x3100, 16, 1, 2},
			{ramBase + 0x3200, 17, 1, 3}, {ramBase + 0x3300, 1, 2, 0}}, 0, 1)
		binary.LittleEndian.PutUint32(r[0x3000:], tipo)
		binary.LittleEndian.PutUint64(r[0x3100:], 8)
		binary.LittleEndian.PutUint32(r[0x3108:], 64)
		binary.LittleEndian.PutUint64(r[0x3200:], 1<<63)
		binary.LittleEndian.PutUint32(r[0x3208:], 0xffffffff)
		binary.LittleEndian.PutUint32(r[0x320c:], flags)
		return r
	}
	// virtio-fs (dev 5): una petición FUSE buena (cabecera de 40 B y sitio
	// para la respuesta), la misma por una tabla indirecta, un FORGET (sin
	// respuesta), una cabecera corta, una indirecta dentro de otra, una
	// lectura tras una escritura y el mismo head dos veces en vuelo.
	fuse := func(ram []byte, at int, opcode uint32) {
		binary.LittleEndian.PutUint32(ram[at:], 40)
		binary.LittleEndian.PutUint32(ram[at+4:], opcode)
		binary.LittleEndian.PutUint64(ram[at+8:], 0x1234)
	}
	vfsBuena := ramCon([][4]uint64{{ramBase + 0x3000, 40, 1, 1}, {ramBase + 0x3100, 256, 2, 0}}, 0, 0)
	fuse(vfsBuena, 0x3000, 1)
	vfsInd := ramCon([][4]uint64{{ramBase + 0x3800, 32, 4, 0}}, 0)
	for i, d := range [][4]uint64{{ramBase + 0x3000, 40, 1, 1}, {ramBase + 0x3100, 256, 2, 0}} {
		o := 0x3800 + i*16
		binary.LittleEndian.PutUint64(vfsInd[o:], d[0])
		binary.LittleEndian.PutUint32(vfsInd[o+8:], uint32(d[1]))
		binary.LittleEndian.PutUint16(vfsInd[o+12:], uint16(d[2]))
		binary.LittleEndian.PutUint16(vfsInd[o+14:], uint16(d[3]))
	}
	fuse(vfsInd, 0x3000, 3)
	vfsForget := ramCon([][4]uint64{{ramBase + 0x3000, 48, 0, 0}}, 0)
	fuse(vfsForget, 0x3000, 2)
	vfsCorta := ramCon([][4]uint64{{ramBase + 0x3000, 16, 1, 1}, {ramBase + 0x3100, 256, 2, 0}}, 0)
	vfsIndInd := ramCon([][4]uint64{{ramBase + 0x3800, 16, 4, 0}}, 0)
	binary.LittleEndian.PutUint64(vfsIndInd[0x3800:], ramBase+0x3800)
	binary.LittleEndian.PutUint32(vfsIndInd[0x3808:], 16)
	binary.LittleEndian.PutUint16(vfsIndInd[0x380c:], 4)
	vfsAlReves := ramCon([][4]uint64{{ramBase + 0x3100, 256, 3, 1}, {ramBase + 0x3000, 40, 0, 0}}, 0)
	for _, ram := range [][]byte{vfsBuena, vfsInd, vfsForget, vfsCorta, vfsIndInd, vfsAlReves} {
		for _, num := range []uint16{1, 2, 256} {
			s = append(s, cola(5, num, 0, 0x1000, 0x2000, ram))
		}
	}
	// Anillos desalineados (used a 2 bytes de su alineación): los accesos de
	// 32 bits a él eran comportamiento indefinido (UBSan, en vq_push).
	s = append(s, cola(5, 2, 0, 0x1000, 0x2002, vfsBuena), cola(0, 2, 0, 0x1001, 0x2000, vfsBuena))
	for dev := byte(0); dev < 6; dev++ {
		for _, num := range []uint16{0, 1, 2, 256, 257, 0xffff} {
			for _, ram := range [][]byte{ciclo, indirecta, enorme, fuera, blkFuera, sinEstado,
				descarte(11, 0), descarte(11, 1), descarte(13, 1), descarte(13, 0xfe)} {
				s = append(s, cola(dev, num, 0, 0x1000, 0x2000, ram))
			}
		}
		// Anillos encima de la tabla, y en el borde de la RAM.
		s = append(s, cola(dev, 4, 0, 0, 0, ciclo))
		s = append(s, cola(dev, 256, 0x100000-16, 0x100000-6, 0x100000-6, ciclo))
	}
	s = append(s, rxSolapada())
	return s
}

// rxSolapada es una cadena de rx de 256 descriptores escribibles que se
// solapan sobre la misma RAM (la grande, de 16 MiB) y suman casi 4 GiB. El
// recvmsg de Darwin rechaza con EINVAL una suma de iov_len mayor que
// INT_MAX, y con eso moría el hilo de recepción: el invitado se quedaba sin
// red para siempre.
func rxSolapada() []byte {
	const num = 256
	descs := make([][4]uint64, num)
	for i := range descs {
		flags, next := uint64(3), uint64(i+1) // NEXT | WRITE
		if i == num-1 {
			flags, next = 2, 0
		}
		descs[i] = [4]uint64{ramBase + 0x4000, 16<<20 - 0x4000, flags, next}
	}
	b := cola(1, num, 0, 0x1000, 0x2000, ramCon(descs, 0))
	b[1] = 1 // la RAM grande (prueba.c)
	return b
}

func TestRxCadenaDeMasDe2GiB(t *testing.T) {
	if e := probarColas(rxSolapada()); e != 0 {
		t.Fatalf("the rx thread would stop with errno %d: the guest would lose its network", e)
	}
}

// Las semillas corren siempre, también sin -fuzz.
func TestColasHostiles(t *testing.T) {
	for i, s := range semillas() {
		if e := probarColas(s); e != 0 {
			t.Errorf("seed %d: the rx thread would stop with errno %d", i, e)
		}
	}
}

// go test -fuzz FuzzColas ./internal/hvvm: cualquier entrada es un invitado
// que programa un dispositivo y le hace notify; no debe romper el proceso.
func FuzzColas(f *testing.F) {
	for _, s := range semillas() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if e := probarColas(data); e != 0 {
			t.Fatalf("the rx thread would stop with errno %d", e)
		}
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

// Un invitado que cambia el QueueNum desde otro vCPU mientras el hilo del
// disco atiende la cola: con el número vivo en vez del copiado, el
// avail_event se escribía 2 KiB más allá de la RAM (aquí, en la página de
// guarda de prueba.c, y el proceso muere).
func TestColaCambiaDeTamanoAMitad(t *testing.T) {
	n := carreraCola(100000)
	switch {
	case n == -2:
		t.Fatal("QueueNum changed while the queue was ready")
	case n < 0:
		t.Fatalf("could not set up the test (%d)", n)
	case n == 0:
		t.Fatal("no request was answered")
	}
}

// El FLUSH de un disco que no es desechable es un F_FULLFSYNC (el fsync de
// macOS no vacía la caché del SSD) y sobre un fichero de APFS tiene que ir bien.
func TestFlushDelDisco(t *testing.T) {
	if e := flushDisco(); e != 0 {
		t.Fatalf("FLUSH status = %d, want 0", e)
	}
}

// used.idx lo escribe solo el dispositivo (virtio 2.7.8): uno que lo leía de
// la RAM seguía desde donde lo dejara el invitado, y el estado del
// dispositivo dependía de lo que escribiera él.
func TestUsedIdxEsDelDispositivo(t *testing.T) {
	switch r := usedDelDispositivo(); r {
	case 0:
	case -1:
		t.Fatal("the first request did not land in used entry 0")
	case -2:
		t.Fatal("after the guest overwrote used.idx, the device followed it instead of its own counter")
	case -3:
		t.Fatal("the snapshot state does not carry used_idx, or restore followed the guest RAM")
	case -4:
		t.Fatal("an old snapshot (no used_idx in its state) did not take used.idx from its RAM")
	case -5:
		t.Fatal("re-arming the queue did not reset used_idx")
	default:
		t.Fatalf("step %d failed", r)
	}
}

// poll de Darwin puede fallar con EAGAIN (no pudo reservar memoria) y otra
// llamada ir bien. El hilo de recepción de la red y el del vsock terminaban
// con cualquier error que no fuera EINTR: el invitado se quedaba sin red, o
// sin agente, para siempre.
func TestPollEAGAINNoMataLaRed(t *testing.T) {
	switch r := pollRed(); {
	case r == -100:
		t.Fatal("could not set up the test")
	case r != 0:
		t.Fatal("after poll failed with EAGAIN, no frame reached the guest: the rx thread stopped")
	}
}

func TestPollEAGAINNoMataElVsock(t *testing.T) {
	switch r := pollVsock(); {
	case r == -100:
		t.Fatal("could not set up the test")
	case r != 0:
		t.Fatal("after poll failed with EAGAIN, the vsock thread stopped")
	}
}

// Las esperas con plazo de los dispositivos y los vCPU van con un plazo
// relativo en el reloj monótono, no con una hora de CLOCK_REALTIME: un salto
// del reloj de pared no puede alargarlas. Aquí, que khv_esperar_hasta cumple
// el plazo y que uno ya vencido vuelve sin esperar.
func TestEsperarHastaCumplePlazo(t *testing.T) {
	d, r := esperarHasta(20 * time.Millisecond)
	if r != int(syscall.ETIMEDOUT) || d < 20*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("a 20 ms wait took %v and returned %d", d, r)
	}
	if d, r := esperarHasta(0); r != int(syscall.ETIMEDOUT) || d > 5*time.Millisecond {
		t.Fatalf("an expired wait took %v and returned %d", d, r)
	}
}

// Si hv_gic_set_spi no sube la línea, vmmio_irq no puede apuntarla como
// alta: ningún aviso posterior la volvería a subir.
func TestIrqNivelSoloSiElGICLoCambia(t *testing.T) {
	if n := irqNivelSinGIC(); n != 0 {
		t.Fatalf("hv_gic_set_spi failed (no VM) and the device still records the line as %d", n)
	}
}

// Si el hilo del vsock muere (un poll que falla sin remedio), el dispositivo
// queda muerto: un dial tiene que fallar al momento con -ENODEV. Antes el
// dispositivo seguía activo y cada dial esperaba su plazo entero (2 s, y el
// daemon tardaba tres fallos, ~30 s, en darse cuenta).
func TestVsockMuertoFallaAlMomento(t *testing.T) {
	r, d := vsockMuerto()
	switch r {
	case -100:
		t.Fatal("could not set up the test")
	case 1:
		t.Fatal("the injected poll failure was never consumed")
	case 2:
		t.Fatalf("after the vsock thread stopped, a dial did not fail with ENODEV (took %v)", d)
	case 3:
		t.Fatal("vsock_init did not start the vsock thread again")
	}
	if d > 50*time.Millisecond {
		t.Fatalf("a dial to a dead vsock took %v", d)
	}
	t.Logf("dial to a dead vsock: %v", d)
}
