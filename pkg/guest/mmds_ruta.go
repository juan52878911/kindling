package guest

import "encoding/binary"

// LA RUTA A MMDS SIN `ip`.
//
// SetupMMDSRoute lanzaba `ip route add` en cada arranque en frío: un fork y un
// exec en un invitado recién nacido, con la caché de páginas fría, de 0,7 a 6
// ms medidos en el M4 (kling-hv) justo antes de escuchar en el puerto del
// agente. Y en una imagen sin iproute2 (una distroless) no había ruta. Por
// netlink es una sola llamada al sistema y no depende de la imagen; `ip` queda
// de respaldo si el kernel la rechaza por algo que no sea "ya existe".

// rutaNetlink pone la ruta /32 on-link a dst por la interfaz ifindex.
// Variable para las pruebas: en un Linux de verdad, como root, tocaría la
// tabla de rutas del anfitrión.
var rutaNetlink = rutaNetlinkOS

// Constantes de <linux/netlink.h> y <linux/rtnetlink.h>: las mismas en todas
// las arquitecturas.
const (
	nlmsgHdrLen    = 16
	rtmsgLen       = 12
	rtmNewRoute    = 24
	nlmFRequest    = 0x1
	nlmFAck        = 0x4
	nlmFExcl       = 0x200
	nlmFCreate     = 0x400
	afInet         = 2
	rtTableMain    = 254
	rtprotBoot     = 3
	rtScopeLink    = 253
	rtnUnicast     = 1
	rtaDst         = 1
	rtaOif         = 4
	rtaHdrLen      = 4
	nlmsgErrorType = 2
)

// mensajeRuta es el RTM_NEWROUTE de `ip route add <dst>/32 dev <ifindex>`:
// tabla main, scope link, y CREATE|EXCL, de modo que si ya está el kernel
// contesta EEXIST como a `ip`. Sin dependencias del sistema para poder
// probarlo en cualquier sitio. Netlink va en el orden de bytes del host.
func mensajeRuta(seq uint32, dst [4]byte, ifindex int32) []byte {
	total := nlmsgHdrLen + rtmsgLen + (rtaHdrLen + 4) + (rtaHdrLen + 4)
	b := make([]byte, total)
	le := binary.NativeEndian
	// nlmsghdr
	le.PutUint32(b[0:], uint32(total))
	le.PutUint16(b[4:], rtmNewRoute)
	le.PutUint16(b[6:], nlmFRequest|nlmFAck|nlmFExcl|nlmFCreate)
	le.PutUint32(b[8:], seq)
	// rtmsg: family, dst_len, src_len, tos, table, protocol, scope, type, flags.
	r := b[nlmsgHdrLen:]
	r[0], r[1], r[4], r[5], r[6], r[7] = afInet, 32, rtTableMain, rtprotBoot, rtScopeLink, rtnUnicast
	// RTA_DST
	a := b[nlmsgHdrLen+rtmsgLen:]
	le.PutUint16(a[0:], rtaHdrLen+4)
	le.PutUint16(a[2:], rtaDst)
	copy(a[4:8], dst[:])
	// RTA_OIF
	a = a[8:]
	le.PutUint16(a[0:], rtaHdrLen+4)
	le.PutUint16(a[2:], rtaOif)
	le.PutUint32(a[4:], uint32(ifindex))
	return b
}

// errnoRespuesta lee el errno del NLMSG_ERROR que contesta a seq: 0 es el ACK
// (todo bien). ok es falso si b no trae esa respuesta.
func errnoRespuesta(b []byte, seq uint32) (errno int32, ok bool) {
	le := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		n := int(le.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return 0, false
		}
		if le.Uint16(b[4:]) == nlmsgErrorType && le.Uint32(b[8:]) == seq && n >= nlmsgHdrLen+4 {
			// struct nlmsgerr: int error (negativo), y el mensaje original.
			return -int32(le.Uint32(b[nlmsgHdrLen:])), true
		}
		n = (n + 3) &^ 3
		if n > len(b) {
			break
		}
		b = b[n:]
	}
	return 0, false
}
