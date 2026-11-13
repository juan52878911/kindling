package guest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"strings"
)

// Cómo llega el host al agente por TCP, en cada backend:
//
//   - Firecracker: el daemon conecta a la IP del namespace y una DNAT la
//     lleva a 172.16.0.2 sin tocar el origen, así que el invitado ve la IP
//     del veth del host (172.30.a.b), que alcanza por su ruta por defecto
//     (vía 172.16.0.1, el tap0).
//   - kling-vz y kling-hv: la pila de usuario (vz/internal/vnet) conecta
//     desde la pasarela, 172.16.0.1. Por vsock llegan con el CID 2, que se
//     mira aparte.
//
// En los dos casos la respuesta del invitado sale por la ruta por defecto.
// Eso es lo que se exige: el origen es la pasarela, o su mejor ruta en la
// tabla del invitado es la de por defecto. Un contenedor con red puente, un
// namespace con su veth o cualquier red conectada directamente tiene una ruta
// más específica y queda fuera. Falsear el origen no sirve: el saludo TCP
// contesta por la ruta del origen, que no lleva de vuelta a quien lo falseó.

// rutaIPv4 es una línea de /proc/net/route.
type rutaIPv4 struct {
	dst, mask, gw [4]byte
	flags         uint32
	metric        int
}

const (
	rtfUp      = 0x1
	rtfGateway = 0x2
)

// rutasInvitado lee la tabla principal de rutas IPv4 (variable para los
// tests). Un error deja al host fuera: mejor un 403 que abrir el agente.
var rutasInvitado = func() ([]rutaIPv4, error) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	return parseRutas(b), nil
}

// parseRutas interpreta /proc/net/route: direcciones en hexadecimal con el
// orden de bytes de la máquina. Las líneas que no se entienden (la cabecera
// entre ellas) se saltan.
func parseRutas(b []byte) []rutaIPv4 {
	var rs []rutaIPv4
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		var r rutaIPv4
		ok := hexIPv4(f[1], &r.dst) && hexIPv4(f[2], &r.gw) && hexIPv4(f[7], &r.mask)
		fl, err1 := strconv.ParseUint(f[3], 16, 32)
		met, err2 := strconv.Atoi(f[6])
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		r.flags, r.metric = uint32(fl), met
		rs = append(rs, r)
	}
	return rs
}

// hexIPv4 lee una dirección de /proc/net/route: un uint32 en hexadecimal
// cuyos bytes en memoria (orden de la máquina) son los de la dirección.
func hexIPv4(s string, out *[4]byte) bool {
	if len(s) != 8 {
		return false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return false
	}
	binary.NativeEndian.PutUint32(out[:], uint32(v))
	return true
}

// llegaPorLaPasarela dice si ip es la pasarela de la ruta por defecto o si
// su mejor ruta es la de por defecto (ver arriba).
func llegaPorLaPasarela(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false // el host nunca habla IPv6 con el agente
	}
	rs, err := rutasInvitado()
	if err != nil {
		return false
	}
	var a [4]byte
	copy(a[:], ip4)
	var mejor rutaIPv4
	hay := false
	for _, r := range rs {
		if r.flags&rtfUp == 0 {
			continue
		}
		if esPorDefecto(r) && r.gw == a {
			return true // la pasarela misma
		}
		if !enRed(a, r.dst, r.mask) {
			continue
		}
		lm, lmMejor := unos(r.mask), unos(mejor.mask)
		if !hay || lm > lmMejor || (lm == lmMejor && r.metric < mejor.metric) {
			mejor, hay = r, true
		}
	}
	return hay && esPorDefecto(mejor)
}

func esPorDefecto(r rutaIPv4) bool {
	return r.dst == [4]byte{} && r.mask == [4]byte{} && r.flags&rtfGateway != 0
}

func enRed(a, dst, mask [4]byte) bool {
	for i := range a {
		if a[i]&mask[i] != dst[i] {
			return false
		}
	}
	return true
}

// unos cuenta los bits a 1 de una máscara.
func unos(m [4]byte) int {
	n := 0
	for _, b := range m {
		for ; b != 0; b &= b - 1 {
			n++
		}
	}
	return n
}
