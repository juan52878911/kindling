package main

// rtnetlink mínimo, sin dependencias: lo justo para la red veth de Android
// (crear el par con un extremo en otro espacio de red, direcciones, subir
// enlaces, ruta por defecto, borrar un enlace). Aquí solo se construyen los
// mensajes (se prueban en cualquier sistema); el envío está en
// netlink_linux.go. Constantes de linux/rtnetlink.h, if_link.h, veth.h.

import (
	"encoding/binary"
	"net"
)

const (
	nlmsgHdrLen = 16

	rtmNewLink  = 16
	rtmDelLink  = 17
	rtmNewAddr  = 20
	rtmNewRoute = 24

	nlmFRequest = 0x1
	nlmFAck     = 0x4
	nlmFExcl    = 0x200
	nlmFCreate  = 0x400
	nlmsgError  = 0x2

	iflaIfname   = 3
	iflaLinkinfo = 18
	iflaNetNsPid = 19
	iflaNetNsFd  = 28
	iflaInfoKind = 1
	iflaInfoData = 2
	vethInfoPeer = 1

	ifaAddress = 1
	ifaLocal   = 2

	rtaGateway = 5
	rtaOif     = 4

	afUnspec = 0
	afInet   = 2

	iffUp = 0x1

	rtTableMain     = 254
	rtProtBoot      = 3
	rtScopeUniverse = 0
	rtnUnicast      = 1
)

// nlAttr es un atributo netlink (tipo + carga ya codificada).
type nlAttr struct {
	typ  uint16
	data []byte
}

func align4(n int) int { return (n + 3) &^ 3 }

func encodeAttrs(attrs []nlAttr) []byte {
	var b []byte
	for _, a := range attrs {
		l := 4 + len(a.data)
		h := make([]byte, 4)
		binary.LittleEndian.PutUint16(h[0:], uint16(l))
		binary.LittleEndian.PutUint16(h[2:], a.typ)
		b = append(b, h...)
		b = append(b, a.data...)
		b = append(b, make([]byte, align4(l)-l)...)
	}
	return b
}

func attrStr(t uint16, s string) nlAttr { return nlAttr{t, append([]byte(s), 0)} }
func attrU32(t uint16, v uint32) nlAttr {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return nlAttr{t, b}
}
func attrNest(t uint16, inner []nlAttr) nlAttr { return nlAttr{t, encodeAttrs(inner)} }

// ifinfomsg: family u8, pad u8, type u16, index i32, flags u32, change u32.
func ifinfomsg(index int32, flags, change uint32) []byte {
	b := make([]byte, 16)
	b[0] = afUnspec
	binary.LittleEndian.PutUint32(b[4:], uint32(index))
	binary.LittleEndian.PutUint32(b[8:], flags)
	binary.LittleEndian.PutUint32(b[12:], change)
	return b
}

// nlMessage arma un mensaje completo (cabecera + cuerpo + atributos).
func nlMessage(typ, flags uint16, seq uint32, body []byte, attrs []nlAttr) []byte {
	payload := append(append([]byte{}, body...), encodeAttrs(attrs)...)
	b := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(payload))
	binary.LittleEndian.PutUint32(b[0:], uint32(nlmsgHdrLen+len(payload)))
	binary.LittleEndian.PutUint16(b[4:], typ)
	binary.LittleEndian.PutUint16(b[6:], flags)
	binary.LittleEndian.PutUint32(b[8:], seq)
	return append(b, payload...)
}

// msgNewVeth: un par veth `name` ↔ `peer`, con `peer` creado directamente en
// el espacio de red del proceso peerPID (el hijo que será init de Android).
func msgNewVeth(seq uint32, name, peer string, peerPID int) []byte {
	peerInfo := append(ifinfomsg(0, 0, 0), encodeAttrs([]nlAttr{
		attrStr(iflaIfname, peer),
		attrU32(iflaNetNsPid, uint32(peerPID)),
	})...)
	return nlMessage(rtmNewLink, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, seq, ifinfomsg(0, 0, 0), []nlAttr{
		attrStr(iflaIfname, name),
		attrNest(iflaLinkinfo, []nlAttr{
			attrStr(iflaInfoKind, "veth"),
			attrNest(iflaInfoData, []nlAttr{{vethInfoPeer, peerInfo}}),
		}),
	})
}

// msgLinkUp sube el enlace de índice idx.
func msgLinkUp(seq uint32, idx int) []byte {
	return nlMessage(rtmNewLink, nlmFRequest|nlmFAck, seq, ifinfomsg(int32(idx), iffUp, iffUp), nil)
}

// msgDelLink borra un enlace por nombre.
func msgDelLink(seq uint32, name string) []byte {
	return nlMessage(rtmDelLink, nlmFRequest|nlmFAck, seq, ifinfomsg(0, 0, 0), []nlAttr{attrStr(iflaIfname, name)})
}

// msgAddAddr: ifaddrmsg (family, prefixlen, flags, scope, index) + dirección.
func msgAddAddr(seq uint32, idx int, ip net.IP, prefix int) []byte {
	body := make([]byte, 8)
	body[0] = afInet
	body[1] = byte(prefix)
	binary.LittleEndian.PutUint32(body[4:], uint32(idx))
	ip4 := ip.To4()
	return nlMessage(rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, seq, body, []nlAttr{
		{ifaLocal, []byte(ip4)}, {ifaAddress, []byte(ip4)},
	})
}

// msgDefaultRoute: 0.0.0.0/0 vía gw por el enlace idx, en la tabla main.
func msgDefaultRoute(seq uint32, idx int, gw net.IP) []byte {
	body := []byte{afInet, 0, 0, 0, rtTableMain, rtProtBoot, rtScopeUniverse, rtnUnicast, 0, 0, 0, 0}
	return nlMessage(rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, seq, body, []nlAttr{
		{rtaGateway, []byte(gw.To4())}, attrU32(rtaOif, uint32(idx)),
	})
}

// nlAckErr lee la respuesta a una petición con NLM_F_ACK: 0 o -errno.
func nlAckErr(b []byte, seq uint32) (errno int32, ok bool) {
	for len(b) >= nlmsgHdrLen {
		l := int(binary.LittleEndian.Uint32(b[0:]))
		if l < nlmsgHdrLen || l > len(b) {
			return 0, false
		}
		typ := binary.LittleEndian.Uint16(b[4:])
		s := binary.LittleEndian.Uint32(b[8:])
		if typ == nlmsgError && s == seq && l >= nlmsgHdrLen+4 {
			return int32(binary.LittleEndian.Uint32(b[nlmsgHdrLen:])), true
		}
		b = b[align4(l):]
	}
	return 0, false
}
