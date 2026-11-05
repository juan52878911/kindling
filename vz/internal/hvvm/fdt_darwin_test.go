//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// propsChosen recorre el bloque de estructura de un DTB y devuelve las
// propiedades de /chosen (nombre → valor).
func propsChosen(t *testing.T, dtb []byte) map[string][]byte {
	t.Helper()
	return propsNodo(t, dtb, "chosen")
}

// propsNodo: las propiedades del nodo hijo de la raíz cuyo nombre empieza por
// prefijo ("chosen", "psci", "intc@").
func propsNodo(t *testing.T, dtb []byte, prefijo string) map[string][]byte {
	t.Helper()
	be := binary.BigEndian
	if len(dtb) < 40 || be.Uint32(dtb) != 0xd00dfeed || int(be.Uint32(dtb[4:])) != len(dtb) {
		t.Fatalf("not a DTB (%d bytes)", len(dtb))
	}
	offSt, offStr := be.Uint32(dtb[8:]), be.Uint32(dtb[12:])
	st := dtb[offSt:offStr]
	strs := dtb[offStr:]
	props := map[string][]byte{}
	var ruta []string
	for i := 0; i+4 <= len(st); {
		tok := be.Uint32(st[i:])
		i += 4
		switch tok {
		case 1: // BEGIN_NODE
			fin := bytes.IndexByte(st[i:], 0)
			ruta = append(ruta, string(st[i:i+fin]))
			i = (i + fin + 1 + 3) &^ 3
		case 2: // END_NODE
			ruta = ruta[:len(ruta)-1]
		case 3: // PROP
			l, no := int(be.Uint32(st[i:])), be.Uint32(st[i+4:])
			i += 8
			if len(ruta) == 2 && strings.HasPrefix(ruta[1], prefijo) {
				nombre := string(strs[no : int(no)+bytes.IndexByte(strs[no:], 0)])
				props[nombre] = append([]byte(nil), st[i:i+l]...)
			}
			i = (i + l + 3) &^ 3
		case 4: // NOP
		case 9: // END
			return props
		default:
			t.Fatalf("bad FDT token %#x at %d", tok, i-4)
		}
	}
	t.Fatal("FDT without END")
	return nil
}

// TestFDTSemillas: /chosen lleva kaslr-seed (u64) y rng-seed (al menos 64
// bytes), y son otras en cada construcción: un arranque en frío no puede
// repetir las del anterior.
func TestFDTSemillas(t *testing.T) {
	a, b := construirFDT(), construirFDT()
	if a == nil || b == nil {
		t.Fatal("fdt_build failed")
	}
	pa, pb := propsChosen(t, a), propsChosen(t, b)
	if string(pa["bootargs"]) != "console=ttyS0\x00" {
		t.Errorf("bootargs = %q", pa["bootargs"])
	}
	for _, p := range []map[string][]byte{pa, pb} {
		if len(p["kaslr-seed"]) != 8 {
			t.Fatalf("kaslr-seed is %d bytes, want 8", len(p["kaslr-seed"]))
		}
		if len(p["rng-seed"]) < 64 {
			t.Fatalf("rng-seed is %d bytes, want >= 64", len(p["rng-seed"]))
		}
		if bytes.Equal(p["rng-seed"], make([]byte, len(p["rng-seed"]))) {
			t.Fatal("rng-seed is all zeros")
		}
	}
	if bytes.Equal(pa["kaslr-seed"], pb["kaslr-seed"]) {
		t.Error("kaslr-seed repeated across two builds")
	}
	if bytes.Equal(pa["rng-seed"], pb["rng-seed"]) {
		t.Error("rng-seed repeated across two builds")
	}
}

// TestFDTAnidada: con EL2 el PSCI va por SMC (un HVC desde EL2 no sale del
// invitado) y el GIC anuncia su interrupción de mantenimiento (PPI 9), que
// KVM necesita para el vGIC; sin EL2 nada de eso cambia.
func TestFDTAnidada(t *testing.T) {
	normal, anidada := construirFDTAnidada(false), construirFDTAnidada(true)
	if normal == nil || anidada == nil {
		t.Fatal("fdt_build failed")
	}
	if m := string(propsNodo(t, normal, "psci")["method"]); m != "hvc\x00" {
		t.Errorf("psci method without EL2 = %q, want hvc", m)
	}
	if _, ok := propsNodo(t, normal, "intc@")["interrupts"]; ok {
		t.Error("the GIC has a maintenance interrupt without EL2")
	}
	if m := string(propsNodo(t, anidada, "psci")["method"]); m != "smc\x00" {
		t.Errorf("psci method with EL2 = %q, want smc", m)
	}
	irq := propsNodo(t, anidada, "intc@")["interrupts"]
	want := []byte{0, 0, 0, 1, 0, 0, 0, 9, 0, 0, 0, 4}
	if !bytes.Equal(irq, want) {
		t.Errorf("GIC interrupts with EL2 = %x, want PPI 9 level (%x)", irq, want)
	}
	// El nodo del timer ya lleva el PPI del hipervisor (10) en los dos.
	for _, d := range [][]byte{normal, anidada} {
		tm := propsNodo(t, d, "timer")["interrupts"]
		if len(tm) != 48 || binary.BigEndian.Uint32(tm[40:]) != 10 {
			t.Errorf("timer interrupts = %x, want the hypervisor PPI 10 last", tm)
		}
	}
}
