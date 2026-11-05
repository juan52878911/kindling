//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// nodosFDT recorre el bloque de estructura de un DTB y devuelve las
// propiedades de cada nodo (ruta → nombre → valor; la raíz es "/").
func nodosFDT(t *testing.T, dtb []byte) map[string]map[string][]byte {
	t.Helper()
	be := binary.BigEndian
	if len(dtb) < 40 || be.Uint32(dtb) != 0xd00dfeed || int(be.Uint32(dtb[4:])) != len(dtb) {
		t.Fatalf("not a DTB (%d bytes)", len(dtb))
	}
	offSt, offStr := be.Uint32(dtb[8:]), be.Uint32(dtb[12:])
	st := dtb[offSt:offStr]
	strs := dtb[offStr:]
	nodos := map[string]map[string][]byte{}
	var ruta []string
	camino := func() string {
		if len(ruta) <= 1 {
			return "/"
		}
		return "/" + strings.Join(ruta[1:], "/")
	}
	for i := 0; i+4 <= len(st); {
		tok := be.Uint32(st[i:])
		i += 4
		switch tok {
		case 1: // BEGIN_NODE
			fin := bytes.IndexByte(st[i:], 0)
			ruta = append(ruta, string(st[i:i+fin]))
			i = (i + fin + 1 + 3) &^ 3
			if nodos[camino()] != nil {
				t.Fatalf("node %s repeated", camino())
			}
			nodos[camino()] = map[string][]byte{}
		case 2: // END_NODE
			ruta = ruta[:len(ruta)-1]
		case 3: // PROP
			l, no := int(be.Uint32(st[i:])), be.Uint32(st[i+4:])
			i += 8
			nombre := string(strs[no : int(no)+bytes.IndexByte(strs[no:], 0)])
			nodos[camino()][nombre] = append([]byte(nil), st[i:i+l]...)
			i = (i + l + 3) &^ 3
		case 4: // NOP
		case 9: // END
			return nodos
		default:
			t.Fatalf("bad FDT token %#x at %d", tok, i-4)
		}
	}
	t.Fatal("FDT without END")
	return nil
}

// propsChosen: las propiedades de /chosen.
func propsChosen(t *testing.T, dtb []byte) map[string][]byte {
	t.Helper()
	return nodosFDT(t, dtb)["/chosen"]
}

// TestFDTSemillas: /chosen lleva kaslr-seed (u64) y rng-seed (al menos 64
// bytes), y son otras en cada construcción: un arranque en frío no puede
// repetir las del anterior.
func TestFDTSemillas(t *testing.T) {
	a, b := construirFDT(2), construirFDT(2)
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

// TestFDTCPUs: un nodo por vCPU con reg = la afinidad de su MPIDR (grupos de
// 8: el noveno es cpu@100), y el GIC con un redistribuidor por vCPU. Con
// hasta 8 vCPU, los nodos de siempre (cpu@0..cpu@7).
func TestFDTCPUs(t *testing.T) {
	be := binary.BigEndian
	for _, n := range []int{1, 8, 12, 64} {
		nodos := nodosFDT(t, construirFDT(n))
		cpus := 0
		for r := range nodos {
			if strings.HasPrefix(r, "/cpus/cpu@") {
				cpus++
			}
		}
		if cpus != n {
			t.Fatalf("%d vCPU: %d cpu nodes", n, cpus)
		}
		for i := 0; i < n; i++ {
			aff := uint32(i%8) | uint32(i/8)<<8
			nodo := nodos[fmt.Sprintf("/cpus/cpu@%x", aff)]
			if nodo == nil {
				t.Fatalf("%d vCPU: no node for cpu%d (cpu@%x)", n, i, aff)
			}
			if reg := nodo["reg"]; len(reg) != 4 || be.Uint32(reg) != aff {
				t.Errorf("%d vCPU: cpu%d reg = %x, want %#x", n, i, reg, aff)
			}
			if uint64(aff) != mpidrAff(i) {
				t.Errorf("cpu%d: FDT affinity %#x and khv_mpidr_aff %#x differ", i, aff, mpidrAff(i))
			}
		}
		var gic map[string][]byte
		for r, p := range nodos {
			if strings.HasPrefix(r, "/intc@") {
				gic = p
			}
		}
		reg := gic["reg"]
		if len(reg) != 32 {
			t.Fatalf("%d vCPU: GIC reg of %d bytes", n, len(reg))
		}
		if rs := uint64(be.Uint32(reg[24:]))<<32 | uint64(be.Uint32(reg[28:])); rs != uint64(n)*0x20000 {
			t.Errorf("%d vCPU: redistributor region %#x, want %#x", n, rs, n*0x20000)
		}
	}
}

// TestMPIDR: khv_mpidr_idx deshace khv_mpidr_aff para cada vCPU de la
// máquina y rechaza lo que no es de ninguno (otro grupo, Aff0 fuera del
// grupo, Aff2 o Aff3, uno de más). Es lo que mira PSCI CPU_ON.
func TestMPIDR(t *testing.T) {
	const n = 64
	for i := 0; i < n; i++ {
		if got := mpidrIdx(mpidrAff(i), n); got != i {
			t.Errorf("cpu%d: aff %#x -> %d", i, mpidrAff(i), got)
		}
	}
	for i := 0; i < 8; i++ {
		if mpidrAff(i) != uint64(i) {
			t.Errorf("cpu%d: MPIDR affinity %#x, want %d (the one the old snapshots carry)", i, mpidrAff(i), i)
		}
	}
	for _, aff := range []uint64{8, 9, 0xff, 0x108, 0x10000, 1 << 32, 0x100 | 1<<32, 0x800} {
		if got := mpidrIdx(aff, n); got != -1 {
			t.Errorf("aff %#x -> %d, want -1", aff, got)
		}
	}
	if got := mpidrIdx(mpidrAff(10), 10); got != -1 {
		t.Errorf("cpu10 in a 10-vCPU machine -> %d, want -1", got)
	}
	if m := maxVCPUsC(); m < 1 || m > n {
		t.Errorf("khv_max_vcpus = %d", m)
	} else {
		t.Logf("khv_max_vcpus = %d", m)
	}
}
