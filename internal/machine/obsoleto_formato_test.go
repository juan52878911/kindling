package machine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// memHV escribe un mem.file de kling-hv con la disposición de vm.c: RAM,
// cabecera (magic, version...) en hdr_off y el pie (magic, ram_size,
// hdr_off, hdr_len) al final.
func memHV(t *testing.T, dir string, magia uint64, version uint32) {
	t.Helper()
	const ram, hdrLen = 64, 40
	b := make([]byte, ram+hdrLen+32)
	binary.LittleEndian.PutUint64(b[ram:], magia)
	binary.LittleEndian.PutUint32(b[ram+8:], version)
	pie := b[ram+hdrLen:]
	binary.LittleEndian.PutUint64(pie[0:], magia)
	binary.LittleEndian.PutUint64(pie[8:], ram)
	binary.LittleEndian.PutUint64(pie[16:], ram)
	binary.LittleEndian.PutUint64(pie[24:], hdrLen)
	if err := os.WriteFile(filepath.Join(dir, "mem.file"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCabeceraHV(t *testing.T) {
	dir := t.TempDir()
	memHV(t, dir, magiaHV, 2)
	m, v, err := cabeceraHV(filepath.Join(dir, "mem.file"))
	if err != nil || m != magiaHV || v != 2 || magiaTexto(m) != "KLNGHV02" {
		t.Fatalf("cabecera = %x %d %v", m, v, err)
	}
	os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 100), 0o600)
	if _, _, err := cabeceraHV(filepath.Join(dir, "mem.file")); err == nil {
		t.Fatal("un fichero sin la magia de kling-hv se leyó como suyo")
	}
}

// causaObsoletoEn: la arquitectura y el formato del volcado de kling-vz y
// kling-hv, además del VMM.
func TestCausaObsoletoEn(t *testing.T) {
	m := newTestManager(t)
	m.FijarOrigen("0.21.0", "kling-hv 0.21.0")
	otra := "amd64"
	if runtime.GOARCH == "amd64" {
		otra = "arm64"
	}
	if c := m.causaObsoletoEn("kling-hv 0.21.0", otra, ""); !strings.Contains(c, otra+" host") {
		t.Errorf("otra arquitectura: %q", c)
	}
	if c := m.causaObsoletoEn("kling-hv 0.21.0", runtime.GOARCH, ""); c != "" {
		t.Errorf("misma arquitectura: %q", c)
	}

	dir := t.TempDir()
	memHV(t, dir, magiaHV, versionHVMax)
	if c := m.causaObsoletoEn("kling-hv 0.20.0", "", dir); c != "" {
		t.Errorf("formato que se lee: %q", c)
	}
	memHV(t, dir, magiaHV, versionHVMax+1)
	if c := m.causaObsoletoEn("kling-hv 0.22.0", "", dir); !strings.Contains(c, "v3") || !strings.HasPrefix(c, "made with") {
		t.Errorf("versión de cabecera mayor: %q", c)
	}
	memHV(t, dir, magiaHVPrefix<<16|0x3033, 1) // "KLNGHV03"
	if c := m.causaObsoletoEn("", "", dir); !strings.Contains(c, "KLNGHV03") {
		t.Errorf("otra generación de la magia (VMM del daemon): %q", c)
	}

	m.FijarOrigen("0.21.0", "kling-vz 0.21.0")
	vz := t.TempDir()
	os.WriteFile(filepath.Join(vz, "snap.file"), []byte(`{"kling_vz": 3}`), 0o600)
	if c := m.causaObsoletoEn("kling-vz 0.20.0", "", vz); c != "" {
		t.Errorf("kling_vz 3: %q", c)
	}
	os.WriteFile(filepath.Join(vz, "snap.file"), []byte(`{"kling_vz": 4}`), 0o600)
	if c := m.causaObsoletoEn("kling-vz 0.22.0", "", vz); !strings.Contains(c, "format 4") {
		t.Errorf("kling_vz 4: %q", c)
	}
	// Firecracker: solo VMM y arquitectura; no se lee nada del volcado.
	m.FijarOrigen("0.21.0", "Firecracker v1.12.0")
	if c := m.causaObsoletoEn("firecracker 1.12.1", "", vz); c != "" {
		t.Errorf("firecracker con parche: %q", c)
	}
}

// cabeceraHV y causaFormatoVolcado con mem.file rotos: lo que no se entiende no
// marca el dorado como obsoleto (lo dirá la restauración), y otra generación
// de la magia con otra disposición se reconoce solo por la magia.
func TestCabeceraHVTruncadaOMala(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "mem.file")
	memHV(t, dir, magiaHV, versionHVMax)
	bueno, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	// Truncado: sin el pie, o solo con el pie (la cabecera se cortó).
	for _, b := range [][]byte{bueno[:31], bueno[:len(bueno)-32], bueno[len(bueno)-32:]} {
		os.WriteFile(ruta, b, 0o600)
		m, v, err := cabeceraHV(ruta)
		if v != 0 {
			t.Errorf("%d bytes: version %d read from a truncated file", len(b), v)
		}
		if err == nil && m != magiaHV {
			t.Errorf("%d bytes: magic %x with no error", len(b), m)
		}
		if c := causaFormatoVolcado(dir, "kling-hv"); c != "" {
			t.Errorf("%d bytes: truncated file marked stale: %q", len(b), c)
		}
	}
	// La cabecera no lleva la magia del pie: magia sí, versión no.
	malo := bytes.Clone(bueno)
	binary.LittleEndian.PutUint64(malo[64:], 0x1122334455667788)
	os.WriteFile(ruta, malo, 0o600)
	if m, v, err := cabeceraHV(ruta); err != nil || m != magiaHV || v != 0 {
		t.Errorf("header with another magic: %x %d %v", m, v, err)
	}
	if c := causaFormatoVolcado(dir, "kling-hv"); c != "" {
		t.Errorf("header with another magic marked stale: %q", c)
	}
	// Otra generación con una cabecera que no se sabe leer: obsoleto por la magia.
	otra := bytes.Clone(bueno)
	binary.LittleEndian.PutUint64(otra[len(otra)-32:], magiaHVPrefix<<16|0x3033)
	binary.LittleEndian.PutUint64(otra[len(otra)-8:], 4) // hdr_len de nadie
	os.WriteFile(ruta, otra, 0o600)
	if c := causaFormatoVolcado(dir, "kling-hv"); !strings.Contains(c, "KLNGHV03") {
		t.Errorf("another generation: %q", c)
	}
	// Ni un fichero que falta.
	os.Remove(ruta)
	if c := causaFormatoVolcado(dir, "kling-hv"); c != "" {
		t.Errorf("missing mem.file: %q", c)
	}
}

// explicarRestauracion traduce los errores de kling-hv como los de kling-vz.
func TestExplicarRestauracionHV(t *testing.T) {
	m := newTestManager(t)
	for _, e := range []string{
		"restore: /x/mem.file is not a kling-hv memory file (or was written by another version)",
		"restore: /x/mem.file: snapshot state version 3, this kling-hv reads 2",
	} {
		err := m.explicarRestauracion(errors.New(e), "svc", &api.Snapshot{Name: "svc"})
		if !errors.Is(err, ErrDoradoObsoleto) || !strings.Contains(err.Error(), "kling-hv with another memory format") {
			t.Errorf("%s → %v", e, err)
		}
	}
}
