package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
)

// LA RED DEL INVITADO SIN ip= DEL KERNEL.
//
// ip_auto_config cuesta ~24 ms del arranque en frío (medido en hv/M4 con
// initcall_debug). Con kling.net=<ip>/<prefijo>,<pasarela> el agente hace lo
// mismo por netlink antes de escuchar: lo y eth0 arriba, la dirección y la ruta
// por defecto, en ese orden. El kernel subía lo dentro de ip_auto_config, así
// que sin ip= hay que subirlo aquí o localhost deja de responder.

const (
	netBootParam = "kling.net"

	rtmNewLink  = 16
	rtmNewAddr  = 20
	ifaHdrLen   = 8
	ifinfoLen   = 16
	ifaAddress  = 1
	ifaLocal    = 2
	ifaBcast    = 4
	rtaGateway  = 5
	iffUp       = 1
	rtScopeUniv = 0
)

// configRed es lo que dice kling.net=.
type configRed struct {
	IP      [4]byte
	Prefijo int
	GW      [4]byte
}

// parseRed lee "<ip>/<prefijo>,<pasarela>" (solo IPv4).
func parseRed(v string) (configRed, error) {
	var c configRed
	a, gw, ok := strings.Cut(v, ",")
	if !ok {
		return c, fmt.Errorf("%s=%q: want <ip>/<prefix>,<gateway>", netBootParam, v)
	}
	p, err := netip.ParsePrefix(a)
	if err != nil || !p.Addr().Is4() {
		return c, fmt.Errorf("%s=%q: bad address", netBootParam, v)
	}
	g, err := netip.ParseAddr(gw)
	if err != nil || !g.Is4() {
		return c, fmt.Errorf("%s=%q: bad gateway", netBootParam, v)
	}
	return configRed{IP: p.Addr().As4(), Prefijo: p.Bits(), GW: g.As4()}, nil
}

func cabeceraNL(b []byte, tipo, flags uint16, seq uint32) {
	le := binary.NativeEndian
	le.PutUint32(b[0:], uint32(len(b)))
	le.PutUint16(b[4:], tipo)
	le.PutUint16(b[6:], flags)
	le.PutUint32(b[8:], seq)
}

func atributoNL(b []byte, tipo uint16, v [4]byte) []byte {
	le := binary.NativeEndian
	le.PutUint16(b[0:], rtaHdrLen+4)
	le.PutUint16(b[2:], tipo)
	copy(b[4:8], v[:])
	return b[8:]
}

// mensajeEnlaceArriba es el RTM_NEWLINK de `ip link set <ifindex> up`.
func mensajeEnlaceArriba(seq uint32, ifindex int32) []byte {
	b := make([]byte, nlmsgHdrLen+ifinfoLen)
	cabeceraNL(b, rtmNewLink, nlmFRequest|nlmFAck, seq)
	le := binary.NativeEndian
	le.PutUint32(b[nlmsgHdrLen+4:], uint32(ifindex))
	le.PutUint32(b[nlmsgHdrLen+8:], iffUp)  // ifi_flags
	le.PutUint32(b[nlmsgHdrLen+12:], iffUp) // ifi_change
	return b
}

// mensajeDireccion es el RTM_NEWADDR de `ip addr add <ip>/<prefijo> brd + dev`.
func mensajeDireccion(seq uint32, ifindex int32, ip [4]byte, prefijo int) []byte {
	b := make([]byte, nlmsgHdrLen+ifaHdrLen+3*(rtaHdrLen+4))
	cabeceraNL(b, rtmNewAddr, nlmFRequest|nlmFAck|nlmFExcl|nlmFCreate, seq)
	h := b[nlmsgHdrLen:]
	h[0], h[1], h[3] = afInet, byte(prefijo), rtScopeUniv
	binary.NativeEndian.PutUint32(h[4:], uint32(ifindex))
	// Difusión: la dirección con los bits de host a uno.
	bc := ip
	m := ^uint32(0)
	if prefijo > 0 {
		m = ^uint32(0) >> uint(prefijo)
	}
	v := binary.BigEndian.Uint32(ip[:]) | m
	binary.BigEndian.PutUint32(bc[:], v)
	a := h[ifaHdrLen:]
	a = atributoNL(a, ifaAddress, ip)
	a = atributoNL(a, ifaLocal, ip)
	atributoNL(a, ifaBcast, bc)
	return b
}

// mensajeRutaDefecto es el RTM_NEWROUTE de `ip route add default via <gw> dev`.
func mensajeRutaDefecto(seq uint32, gw [4]byte, ifindex int32) []byte {
	b := make([]byte, nlmsgHdrLen+rtmsgLen+2*(rtaHdrLen+4))
	cabeceraNL(b, rtmNewRoute, nlmFRequest|nlmFAck|nlmFExcl|nlmFCreate, seq)
	r := b[nlmsgHdrLen:]
	r[0], r[1], r[4], r[5], r[6], r[7] = afInet, 0, rtTableMain, rtprotBoot, rtScopeUniv, rtnUnicast
	a := atributoNL(r[rtmsgLen:], rtaGateway, gw)
	le := binary.NativeEndian
	le.PutUint16(a[0:], rtaHdrLen+4)
	le.PutUint16(a[2:], rtaOif)
	le.PutUint32(a[4:], uint32(ifindex))
	return b
}

// configurarRed aplica v por emisor, que manda un mensaje al kernel y espera su
// ACK, e indice, que da el ifindex de una interfaz. Orden: lo, eth0, dirección,
// ruta. EEXIST no es un fallo (la imagen puede traer ya la red del kernel).
func configurarRed(v string, emisor func(dev string, msg []byte) error, indice func(dev string) (int32, error)) error {
	c, err := parseRed(v)
	if err != nil {
		return err
	}
	lo, err := indice("lo")
	if err != nil {
		return err
	}
	eth, err := indice("eth0")
	if err != nil {
		return err
	}
	pasos := []struct {
		dev string
		msg []byte
	}{
		{"lo", mensajeEnlaceArriba(1, lo)},
		{"eth0", mensajeEnlaceArriba(2, eth)},
		{"eth0", mensajeDireccion(3, eth, c.IP, c.Prefijo)},
		{"eth0", mensajeRutaDefecto(4, c.GW, eth)},
	}
	for _, p := range pasos {
		if err := emisor(p.dev, p.msg); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("network %s: %w", p.dev, err)
		}
	}
	return nil
}

// SetupNet aplica kling.net= si la línea de comandos lo trae. Devuelve false si
// no había parámetro (la red la puso el kernel con ip=).
func SetupNet() (bool, error) {
	v, ok := cmdlineParams().values[netBootParam]
	if !ok {
		return false, nil
	}
	return true, configurarRedOS(v)
}
