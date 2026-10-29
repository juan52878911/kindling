package machine

// El meta.json de un dorado: su versión, lo que no conoce y con qué se hizo.
//
// VERSIÓN. Desde la v1 lleva "schema": 1 (pkg/esquema). Sin el campo es la v0,
// lo de hasta v0.17: se lee igual y la primera vez que el daemon lo reescribe
// (una anotación, unas credenciales) deja antes la copia meta.json.v0.bak. Uno
// con un schema mayor lo escribió un kling más nuevo: no se restaura, no se
// anota y no se aparta; se falla diciendo qué versión encontró.
//
// LO QUE NO CONOCE. Un binario viejo que lee el meta a su struct y lo vuelve a
// escribir perdía en silencio los campos de uno más nuevo. Al reescribir se
// parte del fichero que hay y se conservan las claves que api.Snapshot no
// tiene: lo que no se entiende se deja como estaba.
//
// CON QUÉ SE HIZO. Un dorado es memoria volcada por un VMM concreto; otro
// Firecracker puede no saber cargarla, y el fallo sale al despertar, con un
// error crudo lejos de la causa. El meta guarda el VMM y su versión, la de
// kling y, en macOS, la del sistema. Al leerlo se compara con lo que corre
// ahora (causaObsoleto) y, si no casa de una forma que se sabe que rompe, el
// dorado sale "stale" en el listado y runFrom se niega con la orden para
// rehacerlo en vez de intentarlo a ciegas.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// metaSchema es la versión de meta.json que escribe este binario.
//
// v2: el overlay va en rootfs_digest (sha256 en árbol, pkg/digest.Tree) y no
// en rootfs_sha256. Un kling de v1 no lo comprobaría y su firma no casaría:
// por eso sube la versión, y ese kling se niega diciendo cuál encontró.
const metaSchema = 2

// ErrDoradoObsoleto es que el dorado se hizo con un VMM que el de ahora no
// sabe restaurar: hay que rehacerlo.
var ErrDoradoObsoleto = errors.New("stale golden snapshot")

// metaEnDisco es meta.json tal cual se escribe: la cabecera de versión delante
// y el snapshot después.
type metaEnDisco struct {
	esquema.Cabecera
	*api.Snapshot
}

// clavesV04 son los campos con los que v0.4 guardaba en el meta el catálogo y
// la salud de MCP. Desde v0.5 son las anotaciones mcp.tools y mcp.health, y
// hasta v0.17 un meta con ellos se elevaba a anotaciones al leerlo. Ya no
// (docs/actualizar.md §5, PR 11): un meta v0 con alguno es de v0.4 y se
// rechaza diciéndolo, en vez de perder en silencio lo que llevaba.
var clavesV04 = []string{"tools", "tools_at", "health", "health_at", "health_err"}

// clavesConocidas son las claves JSON de api.Snapshot y la del esquema: lo que
// este binario sabe escribir. Lo demás de un meta es de otro binario.
var clavesConocidas = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{"schema": true}
	t := reflect.TypeOf(api.Snapshot{})
	for i := 0; i < t.NumField(); i++ {
		nombre, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if nombre != "" && nombre != "-" {
			out[nombre] = true
		}
	}
	return out
})

// decodificarMeta lee un meta.json de cualquier versión que entienda este
// binario y devuelve la que tenía. ruta solo sirve para los mensajes.
func decodificarMeta(ruta string, b []byte) (*api.Snapshot, int, error) {
	v, err := esquema.Comprobar(ruta, b, metaSchema)
	if err != nil {
		return nil, v, err
	}
	// v0 y v1 tienen los mismos campos: la v1 solo añade la cabecera y los
	// del origen, que en un v0 quedan vacíos ("no consta").
	if v == 0 {
		if err := comprobarMetaV04(ruta, b); err != nil {
			return nil, v, err
		}
	}
	var s api.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, v, err
	}
	s.Stale = "" // se calcula al leer, nunca viene del disco
	return &s, v, nil
}

// comprobarMetaV04 da error si el meta lleva los campos de MCP de v0.4.
func comprobarMetaV04(ruta string, b []byte) error {
	var claves map[string]json.RawMessage
	if json.Unmarshal(b, &claves) != nil {
		return nil // lo ilegible lo dice el decodificado de después
	}
	for _, k := range clavesV04 {
		if _, ok := claves[k]; ok {
			return fmt.Errorf("%s was written by kling v0.4 (it has %q), which this kling no longer reads: "+
				"save the template again (kling save), or annotate it once with kling v0.17 to move its MCP fields to annotations", ruta, k)
		}
	}
	return nil
}

// codificarMeta escribe s como meta.json de la versión actual. previo es el
// meta que hay en disco (nil en un dorado nuevo): sus claves desconocidas se
// conservan, al final y en orden.
func codificarMeta(s *api.Snapshot, previo []byte) ([]byte, error) {
	c := *s
	c.Stale = ""
	b, err := json.MarshalIndent(metaEnDisco{esquema.Cabecera{Schema: metaSchema}, &c}, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(previo) == 0 {
		return b, nil
	}
	var viejo map[string]json.RawMessage
	if json.Unmarshal(previo, &viejo) != nil {
		return b, nil
	}
	conocidas := clavesConocidas()
	var extra []string
	for k := range viejo {
		if !conocidas[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return b, nil
	}
	sort.Strings(extra)
	var out bytes.Buffer
	// MarshalIndent acaba en "\n}": se quita la llave y se siguen añadiendo
	// claves al mismo nivel.
	out.Write(bytes.TrimRight(bytes.TrimSuffix(b, []byte("}")), " \n"))
	for _, k := range extra {
		nombre, _ := json.Marshal(k)
		out.WriteString(",\n  ")
		out.Write(nombre)
		out.WriteString(": ")
		if err := json.Indent(&out, viejo[k], "  ", "  "); err != nil {
			return nil, err
		}
	}
	out.WriteString("\n}")
	if !json.Valid(out.Bytes()) {
		return nil, fmt.Errorf("meta.json: merging unknown keys produced invalid JSON")
	}
	return out.Bytes(), nil
}

// origenHost es con qué trabaja este daemon: lo que se graba en cada dorado y
// contra lo que se comparan los que ya hay.
type origenHost struct {
	kling string // versión de kling, sin la v
	vmm   string // "firecracker 1.12.0", "kling-vz 0.18.0"; "" si no se sabe
	macOS string // "26.1" en macOS; "" en Linux
}

// FijarOrigen dice al manager qué versión de kling es y qué contesta su VMM a
// --version (tal cual, "Firecracker v1.12.0"). El daemon lo llama al crearlo;
// sin llamarlo (los tests) nada se graba y ningún dorado sale obsoleto.
func (m *Manager) FijarOrigen(kling, vmmVersion string) {
	m.origen.Store(&origenHost{kling: strings.TrimPrefix(kling, "v"), vmm: normalizarVMM(vmmVersion), macOS: versionMacOS()})
}

func (m *Manager) origenActual() origenHost {
	if o := m.origen.Load(); o != nil {
		return *o
	}
	return origenHost{}
}

// grabarOrigen pone en s con qué se hace el dorado a partir de mc: el VMM es
// el de la máquina, no el del daemon (ver vmmDe).
func (m *Manager) grabarOrigen(s *api.Snapshot, mc *api.Machine) {
	o := m.origenActual()
	s.VMM, s.MacOS, s.KlingVersion, s.Arch = m.vmmDe(mc), o.macOS, o.kling, runtime.GOARCH
}

// vmmDe es el VMM que corre mc: el que la lanzó (Machine.VMM), que no tiene
// por qué ser el del daemon. Con `daemon.vmm: hv` puesto a mano sobre una raíz
// con máquinas de kling-vz vivas, el daemon nuevo las readopta, pero quien
// vuelca su memoria sigue siendo kling-vz: sellarla como kling-hv hacía que
// el thaw la cargara en un VMM que no entiende su formato. Una máquina de
// antes de este campo (vacío) cae al del daemon, como hasta ahora.
func (m *Manager) vmmDe(mc *api.Machine) string {
	if mc != nil && mc.VMM != "" {
		return mc.VMM
	}
	return m.origenActual().vmm
}

// normalizarVMM deja la primera línea de `<vmm> --version` como "nombre
// versión": "Firecracker v1.12.0" → "firecracker 1.12.0".
func normalizarVMM(s string) string {
	linea, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	campos := strings.Fields(linea)
	if len(campos) < 2 {
		return ""
	}
	return strings.ToLower(campos[0]) + " " + strings.TrimPrefix(campos[1], "v")
}

// menorDe devuelve "1.12" de "1.12.0", o "" si no es una versión.
func menorDe(v string) string {
	partes := strings.SplitN(v, ".", 3)
	if len(partes) < 2 {
		return ""
	}
	for _, p := range partes[:2] {
		if _, err := strconv.Atoi(p); err != nil {
			return ""
		}
	}
	return partes[0] + "." + partes[1]
}

// causaObsoleto dice por qué un dorado hecho con hecho no se puede restaurar
// con ahora, o "" si se puede o no se sabe. Solo cuenta lo que se sabe que
// rompe:
//
//   - otro VMM (un dorado de Firecracker en kling-vz, o al revés): no hay
//     traducción posible.
//   - otra MAJOR.MINOR de Firecracker: el formato del volcado es suyo y lo sube
//     en versiones menores; un parche no lo cambia, así que 1.12.0 → 1.12.3 no
//     invalida nada.
//
// kling-vz no se compara por su versión: su volcado lleva su propio número de
// formato ("kling_vz" en snap.file) y el propio kling-vz rechaza el que no
// entiende, con un error que explicarRestauracion traduce. Tampoco la versión
// de macOS: no hay regla conocida de cuándo Virtualization deja de aceptar un
// estado guardado, así que solo se menciona si la restauración falla.
func causaObsoleto(hecho, ahora string) string {
	if hecho == "" || ahora == "" {
		return ""
	}
	nh, vh, _ := strings.Cut(hecho, " ")
	na, va, _ := strings.Cut(ahora, " ")
	if nh != na {
		return fmt.Sprintf("made with %s; this host runs %s", hecho, ahora)
	}
	if nh == "firecracker" {
		mh, ma := menorDe(vh), menorDe(va)
		if mh != "" && ma != "" && mh != ma {
			return fmt.Sprintf("made with %s; this host runs %s", hecho, ahora)
		}
	}
	return ""
}

// marcarObsoleto rellena s.Stale con la causa, si la hay.
func (m *Manager) marcarObsoleto(s *api.Snapshot) {
	s.Stale = m.causaObsoletoEn(s.VMM, s.Arch, m.snapDir(s.Name))
}

// causaObsoletoEn es causaObsoleto con lo que no cabe en la versión del VMM:
// la arquitectura (arch, la del host que hizo el volcado; un volcado no se
// restaura en otra) y el formato del volcado que hay en dir, de kling-vz o
// kling-hv. Su versión no se compara (los dos se actualizan con el daemon),
// sino lo que importa de ella: el formato que escribió y si el de ahora lo lee.
// Así un dorado de un kling-hv más nuevo sale "stale" con su causa en vez de
// fallar al restaurar con un error del VMM. dir "" no mira el formato.
func (m *Manager) causaObsoletoEn(vmm, arch, dir string) string {
	if arch != "" && arch != runtime.GOARCH {
		return fmt.Sprintf("made with an %s host; this host is %s", arch, runtime.GOARCH)
	}
	ahora := m.origenActual().vmm
	if c := causaObsoleto(vmm, ahora); c != "" {
		return c
	}
	if dir == "" {
		return ""
	}
	nombre := vmm
	if nombre == "" {
		nombre = ahora
	}
	nombre, _, _ = strings.Cut(nombre, " ")
	return causaFormatoVolcado(dir, nombre)
}

// Formatos de volcado que lee el VMM que va con este daemon (kling-vz y
// kling-hv se publican y actualizan juntos con él). Tienen que casar con
// vz/internal/spec.FormatVersion y con SNAP_MAGIC/SNAP_VERSION de
// vz/internal/hvvm/vm.c (que fija TestCabeceraKLNGHV en ese paquete).
const (
	formatoVZMax  = 3
	magiaHV       = 0x4b4c4e4748563032 // "KLNGHV02"
	magiaHVPrefix = 0x4b4c4e474856     // "KLNGHV", sin la generación
	versionHVMax  = 2
)

// causaFormatoVolcado mira el volcado de dir: "kling_vz" de snap.file para
// kling-vz, la cabecera de mem.file para kling-hv. Devuelve la causa si es de
// un formato que este no lee, o "" si lo lee o no se sabe (lo que no se
// entiende lo dirá la restauración).
func causaFormatoVolcado(dir, vmm string) string {
	switch vmm {
	case "kling-vz":
		b, err := os.ReadFile(filepath.Join(dir, "snap.file"))
		if err != nil {
			return ""
		}
		var f struct {
			Formato int `json:"kling_vz"`
		}
		if json.Unmarshal(b, &f) == nil && f.Formato > formatoVZMax {
			return fmt.Sprintf("made with kling-vz snapshot format %d; this host's kling-vz reads up to %d", f.Formato, formatoVZMax)
		}
	case "kling-hv":
		magia, version, err := cabeceraHV(filepath.Join(dir, "mem.file"))
		if err != nil {
			return ""
		}
		if magia != magiaHV {
			return fmt.Sprintf("made with kling-hv memory format %q; this host's kling-hv reads %q", magiaTexto(magia), magiaTexto(magiaHV))
		}
		if version > versionHVMax {
			return fmt.Sprintf("made with kling-hv memory format %s v%d; this host's kling-hv reads up to v%d", magiaTexto(magia), version, versionHVMax)
		}
	}
	return ""
}

// cabeceraHV lee la magia y la versión del mem.file de kling-hv: al final va
// el pie (magic, ram_size, hdr_off, hdr_len, uint64 little-endian) y en
// hdr_off la cabecera (magic uint64, version uint32...), ver vm.c. Un fichero
// que no lleva la magia "KLNGHV" (de otro VMM, truncado) es un error: no es
// de kling-hv y no hay formato que comparar.
func cabeceraHV(ruta string) (magia uint64, version uint32, err error) {
	f, err := os.Open(ruta)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var pie [32]byte
	if fi.Size() < int64(len(pie)) {
		return 0, 0, errors.New("too short for a kling-hv memory file")
	}
	if _, err := f.ReadAt(pie[:], fi.Size()-int64(len(pie))); err != nil {
		return 0, 0, err
	}
	magia = binary.LittleEndian.Uint64(pie[0:])
	if magia>>16 != magiaHVPrefix {
		return 0, 0, errors.New("not a kling-hv memory file")
	}
	hdrOff, hdrLen := binary.LittleEndian.Uint64(pie[16:]), binary.LittleEndian.Uint64(pie[24:])
	var cab [12]byte
	if hdrLen < uint64(len(cab)) || hdrOff > uint64(fi.Size())-uint64(len(cab)) {
		return magia, 0, nil // otra generación con otra disposición: basta la magia
	}
	if _, err := f.ReadAt(cab[:], int64(hdrOff)); err != nil {
		return 0, 0, err
	}
	if binary.LittleEndian.Uint64(cab[0:]) != magia {
		return magia, 0, nil
	}
	return magia, binary.LittleEndian.Uint32(cab[8:]), nil
}

// magiaTexto es la magia de kling-hv como se lee ("KLNGHV02").
func magiaTexto(m uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], m)
	return string(b[:])
}

// remedioDorado son las órdenes para rehacer el dorado name.
func remedioDorado(name string) string {
	return fmt.Sprintf("  kling save -replace <machine> %s    (a template saved by hand)\n"+
		"  kling mcp import %s -force    (an imported MCP service)", name, name)
}

// errObsoleto es el error de restaurar un dorado obsoleto.
func errObsoleto(name, causa string) error {
	return fmt.Errorf("%w: golden %q was %s, and its memory dump can't be restored by another VMM version. "+
		"Re-save it:\n%s", ErrDoradoObsoleto, name, causa, remedioDorado(name))
}

// explicarRestauracion añade al fallo de restaurar el dorado s lo que se sabe
// de su origen: si el formato de kling-vz no casa, o si el dorado se hizo con
// otro VMM u otro macOS aunque no se considerara obsoleto. Sin nada que
// añadir, devuelve err tal cual.
func (m *Manager) explicarRestauracion(err error, name string, s *api.Snapshot) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "unsupported snapshot format kling_vz") {
		return fmt.Errorf("%w: golden %q was saved by a kling-vz with another snapshot format. Re-save it:\n%s\nUnderlying error: %v",
			ErrDoradoObsoleto, name, remedioDorado(name), err)
	}
	// Los de kling-hv (vm.c, khv_restore): otra magia o un hdr_len que no
	// casa, u otra versión de la cabecera.
	if e := err.Error(); strings.Contains(e, "is not a kling-hv memory file") || strings.Contains(e, "this kling-hv reads") {
		return fmt.Errorf("%w: golden %q was saved by a kling-hv with another memory format. Re-save it:\n%s\nUnderlying error: %v",
			ErrDoradoObsoleto, name, remedioDorado(name), err)
	}

	o := m.origenActual()
	var dif []string
	if s.VMM != "" && o.vmm != "" && s.VMM != o.vmm {
		dif = append(dif, fmt.Sprintf("%s (now %s)", s.VMM, o.vmm))
	}
	if s.MacOS != "" && o.macOS != "" && s.MacOS != o.macOS {
		dif = append(dif, fmt.Sprintf("macOS %s (now %s)", s.MacOS, o.macOS))
	}
	if len(dif) == 0 {
		return err
	}
	return fmt.Errorf("%w\ngolden %q was made with %s; if it keeps failing, re-save it:\n%s",
		err, name, strings.Join(dif, " and "), remedioDorado(name))
}
