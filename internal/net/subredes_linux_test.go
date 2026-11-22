package net

import (
	"encoding/binary"
	stdnet "net"
	"slices"
	"syscall"
	"testing"
)

func comoTexto(ds []DireccionHost) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.If+" "+d.IP.String())
	}
	slices.Sort(out)
	return out
}

// P4: un solo volcado de direcciones, sea cual sea el número de interfaces,
// y las mismas direcciones que interfaz a interfaz.
func TestDireccionesHostUnSoloVolcado(t *testing.T) {
	viejo := volcarDirecciones
	t.Cleanup(func() { volcarDirecciones = viejo })
	volcados := 0
	volcarDirecciones = func() ([]byte, error) { volcados++; return viejo() }

	ds, err := direccionesHost()
	if err != nil {
		t.Fatal(err)
	}
	if volcados != 1 {
		t.Errorf("%d volcados de direcciones, quería 1", volcados)
	}
	ref, err := direccionesPorInterfaz()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := comoTexto(ds), comoTexto(ref); !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("direcciones =\n%v\nquería (interfaz a interfaz)\n%v", got, want)
	}
}

// Para medir en un netns con muchas interfaces (el lab, con 500 dummy):
// go test -run x -bench DireccionesHost ./internal/net
func BenchmarkDireccionesHostVolcado(b *testing.B) {
	for b.Loop() {
		if _, err := direccionesHost(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDireccionesHostPorInterfaz(b *testing.B) {
	for b.Loop() {
		if _, err := direccionesPorInterfaz(); err != nil {
			b.Fatal(err)
		}
	}
}

// msgDireccion fabrica un RTM_NEWADDR IPv4 con IFA_LOCAL.
func msgDireccion(flags uint16, idx uint32, ip [4]byte) []byte {
	b := make([]byte, 16+8+8)
	binary.NativeEndian.PutUint32(b[0:], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], syscall.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(b[6:], flags)
	b[16] = syscall.AF_INET
	b[17] = 30
	binary.NativeEndian.PutUint32(b[20:], idx)
	binary.NativeEndian.PutUint16(b[24:], 8)
	binary.NativeEndian.PutUint16(b[26:], syscall.IFA_LOCAL)
	copy(b[28:], ip[:])
	return b
}

func loIndice(t *testing.T) uint32 {
	t.Helper()
	it, err := stdnet.InterfaceByName("lo")
	if err != nil {
		t.Skip(err)
	}
	return uint32(it.Index)
}

// S1: un volcado marcado NLM_F_DUMP_INTR es incoherente: se repite.
func TestDireccionesHostRepiteVolcadoInterrumpido(t *testing.T) {
	idx := loIndice(t)
	viejo := volcarDirecciones
	t.Cleanup(func() { volcarDirecciones = viejo })
	n := 0
	volcarDirecciones = func() ([]byte, error) {
		n++
		if n == 1 {
			return msgDireccion(0x10, idx, [4]byte{172, 30, 0, 1}), nil
		}
		return msgDireccion(0, idx, [4]byte{172, 30, 0, 5}), nil
	}
	ds, err := direccionesHost()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(ds) != 1 || ds[0].IP.String() != "172.30.0.5" {
		t.Fatalf("volcados=%d dirs=%v", n, ds)
	}
}

// Si los tres intentos salen interrumpidos, error (no una lista incompleta).
func TestDireccionesHostInterrumpidoSiempreEsError(t *testing.T) {
	idx := loIndice(t)
	viejo := volcarDirecciones
	t.Cleanup(func() { volcarDirecciones = viejo })
	n := 0
	volcarDirecciones = func() ([]byte, error) {
		n++
		return msgDireccion(0x10, idx, [4]byte{172, 30, 0, 1}), nil
	}
	if _, err := direccionesHost(); err == nil || n != 3 {
		t.Fatalf("err=%v volcados=%d, quería error tras 3", err, n)
	}
}
