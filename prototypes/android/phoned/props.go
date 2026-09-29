package main

// Lectura y reescritura de propiedades de Android sin getprop ni setprop.
//
// Las propiedades viven en ficheros de /dev/__properties__ (un tmpfs de
// Android), uno por contexto de SELinux, con el formato de bionic
// (libc/system_properties/include/system_properties/prop_area.h y
// prop_info.h). init los mapea en escritura; todos los demás procesos, en
// lectura y COMPARTIDOS: lo que se escribe en el fichero lo ve cada proceso al
// instante, sin reiniciar nada.
//
// Para qué: (1) la sonda de listo lee sys.boot_completed en microsegundos, sin
// entrar en los espacios de nombres de Android; (2) ro.serialno, que init fija
// al arrancar y ningún setprop puede cambiar después, se reescribe en su sitio
// en cada clon (identity.go), como hace resetprop de Magisk.
//
// Formato (little endian, todo alineado a 4):
//
//	prop_area: bytes_used u32, serial u32, magic u32 ("PROP"), version u32,
//	           reserved[28] u32, data[]            → cabecera de 128 bytes
//	prop_bt:   namelen u32, prop u32, left u32, right u32, children u32, name[]
//	prop_info: serial u32, value[92], name[]
//
// Los desplazamientos de prop_bt y prop_info cuentan desde data. La raíz del
// árbol es el prop_bt en data+0 (sin nombre); justo detrás está el área de
// respaldo ("dirty backup area") que los lectores usan mientras un valor se
// está escribiendo. Un nombre se busca trozo a trozo ("ro", "serialno"): cada
// nivel es un árbol binario ordenado por longitud y luego por bytes.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unsafe"
)

const (
	propAreaMagic   = 0x504f5250
	propAreaVersion = 0xfc6ed0ab
	propHeaderSize  = 128
	propValueMax    = 92
	propBtSize      = 20
	propLongFlag    = 1 << 16
)

var errPropNotFound = errors.New("property not found")

// propArea es un fichero de propiedades mapeado (o, en las pruebas, un búfer).
// b tiene que estar alineado a 4 bytes: los campos serial se leen y escriben
// con operaciones atómicas, como en bionic.
type propArea struct {
	b []byte
	// wake despierta a quien espera en un futex de esa dirección (Linux); nil
	// en las pruebas.
	wake func(addr *uint32)
}

func (a *propArea) valid() error {
	if len(a.b) < propHeaderSize+propBtSize+propValueMax {
		return fmt.Errorf("property area too small (%d bytes)", len(a.b))
	}
	if uintptr(unsafe.Pointer(&a.b[0]))%4 != 0 {
		return errors.New("property area not aligned")
	}
	if m := a.u32(8); m != propAreaMagic {
		return fmt.Errorf("bad property area magic %#x", m)
	}
	if v := a.u32(12); v != propAreaVersion {
		return fmt.Errorf("unsupported property area version %#x", v)
	}
	return nil
}

// ptr devuelve la palabra en el desplazamiento absoluto off, o nil si no cabe.
func (a *propArea) ptr(off uint64) *uint32 {
	if off%4 != 0 || off+4 > uint64(len(a.b)) {
		return nil
	}
	return (*uint32)(unsafe.Pointer(&a.b[off]))
}

func (a *propArea) u32(off uint64) uint32 {
	p := a.ptr(off)
	if p == nil {
		return 0
	}
	return atomic.LoadUint32(p)
}

// data pasa un desplazamiento de data a absoluto.
func data(off uint32) uint64 { return propHeaderSize + uint64(off) }

// btName es el nombre del prop_bt en off (de data), o nil si se sale.
func (a *propArea) btName(off uint32) []byte {
	start := data(off) + propBtSize
	n := uint64(a.u32(data(off)))
	if n > 256 || start+n > uint64(len(a.b)) {
		return nil
	}
	return a.b[start : start+n]
}

func cmpPropName(a, b []byte) int {
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return bytes.Compare(a, b)
}

// findBt busca name entre los hermanos que cuelgan de off. El recorrido
// tiene un tope: un fichero corrupto no puede dejarlo dando vueltas.
func (a *propArea) findBt(off uint32, name []byte) (uint32, bool) {
	for i := 0; i < 1<<16; i++ {
		nm := a.btName(off)
		if nm == nil {
			return 0, false
		}
		c := cmpPropName(name, nm)
		if c == 0 {
			return off, true
		}
		var next uint32
		if c < 0 {
			next = a.u32(data(off) + 8)
		} else {
			next = a.u32(data(off) + 12)
		}
		if next == 0 {
			return 0, false
		}
		off = next
	}
	return 0, false
}

// find devuelve el desplazamiento (de data) del prop_info de name.
func (a *propArea) find(name string) (uint32, error) {
	if err := a.valid(); err != nil {
		return 0, err
	}
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return 0, fmt.Errorf("bad property name %q", name)
	}
	cur := uint32(0)
	for _, seg := range strings.Split(name, ".") {
		ch := a.u32(data(cur) + 16)
		if ch == 0 {
			return 0, errPropNotFound
		}
		var ok bool
		if cur, ok = a.findBt(ch, []byte(seg)); !ok {
			return 0, errPropNotFound
		}
	}
	pi := a.u32(data(cur) + 4)
	if pi == 0 || data(pi)+4+propValueMax > uint64(len(a.b)) {
		return 0, errPropNotFound
	}
	return pi, nil
}

// get lee el valor de name. Como __system_property_read: si el serial cambia
// mientras se copia (o está marcado como sucio), se repite.
func (a *propArea) get(name string) (string, error) {
	pi, err := a.find(name)
	if err != nil {
		return "", err
	}
	sp := a.ptr(data(pi))
	for i := 0; i < 1000; i++ {
		s := atomic.LoadUint32(sp)
		if s&propLongFlag != 0 {
			return "", fmt.Errorf("%s is a long property (not supported)", name)
		}
		n := uint64(s >> 24)
		if n >= propValueMax {
			return "", fmt.Errorf("%s: bad length %d", name, n)
		}
		src := data(pi) + 4
		if s&1 != 0 {
			src = data(propBtSize)
		}
		v := string(a.b[src : src+n])
		if atomic.LoadUint32(sp) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s keeps changing", name)
}

// set reescribe en su sitio el valor de una propiedad que ya existe, con el
// protocolo de SystemProperties::Update de bionic: copia del valor viejo al
// área de respaldo, serial sucio, valor nuevo, serial nuevo (longitud en el
// byte alto, contador +1 en los 24 bajos) y despertar a quien espera.
// Después hay que incrementar el serial global (bumpSerial), que vive en otro
// fichero (properties_serial).
func (a *propArea) set(name, value string) error {
	if len(value) >= propValueMax {
		return fmt.Errorf("value for %s too long (%d bytes, max %d)", name, len(value), propValueMax-1)
	}
	pi, err := a.find(name)
	if err != nil {
		return err
	}
	sp := a.ptr(data(pi))
	s := atomic.LoadUint32(sp)
	if s&propLongFlag != 0 {
		return fmt.Errorf("%s is a long property (not supported)", name)
	}
	old := uint64(s >> 24)
	if old >= propValueMax {
		return fmt.Errorf("%s: bad length %d", name, old)
	}
	val := a.b[data(pi)+4 : data(pi)+4+propValueMax]
	backup := a.b[data(propBtSize) : data(propBtSize)+propValueMax]
	copy(backup, val[:old+1])
	s |= 1
	atomic.StoreUint32(sp, s)
	n := copy(val, value)
	for i := n; i < len(val); i++ {
		val[i] = 0
	}
	s = uint32(len(value))<<24 | ((s + 1) & 0xffffff)
	atomic.StoreUint32(sp, s)
	if a.wake != nil {
		a.wake(sp)
	}
	return nil
}

// bumpSerial incrementa el serial global de un área (el de properties_serial:
// quien espera "cualquier cambio", como __system_property_wait_any, lo mira).
func (a *propArea) bumpSerial() error {
	if err := a.valid(); err != nil {
		return err
	}
	sp := a.ptr(4)
	atomic.AddUint32(sp, 1)
	if a.wake != nil {
		a.wake(sp)
	}
	return nil
}
