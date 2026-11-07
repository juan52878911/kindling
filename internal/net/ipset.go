package net

// Meter una IP en el ipset del allowlist de un netns.
//
// El resolver dinámico (dnsresolver.go) siembra cada IP nueva ANTES de
// contestar al invitado: si contestara antes, su primer SYN llegaría al DROP.
// Con `ip netns exec NS ipset add ...` eran dos procesos (fork+exec de ip, que
// entra en el netns y hace exec de ipset) en el camino de cada respuesta DNS.
// Por netlink es un mensaje y su ACK por un socket que vive en el netns, sin
// procesos. Si netlink no está disponible (otro sistema, un kernel sin el
// subsistema, un netns que no se puede abrir), se cae al comando de siempre.

import (
	"encoding/binary"
	"strconv"
	"syscall"
)

// anadirIPSet mete ip en el set del netns ns, con timeout segundos de vida
// (0: la del set, que en el del allowlist es permanente). Como `-exist`: si ya
// está, renueva su timeout en vez de fallar.
func anadirIPSet(ns, set, ip string, timeout int) error {
	if err := anadirNetlink(ns, set, ip, timeout); err == nil {
		return nil
	}
	args := []string{"ip", "netns", "exec", ns, "ipset", "add", set, ip}
	if timeout > 0 {
		args = append(args, "timeout", strconv.Itoa(timeout))
	}
	return ejecutar(append(args, "-exist")...)
}

// anadirNetlink es la vía rápida de anadirIPSet (ipset_nl_linux.go). Variable
// para los tests.
var anadirNetlink = anadirIPSetNetlink

// Constantes del protocolo de ipset por netlink
// (include/uapi/linux/netfilter/ipset/ip_set.h y nfnetlink.h).
const (
	nfnlSubsysIPSet     = 6
	ipsetCmdAdd         = 9
	ipsetProtocol       = 6 // IPSET_PROTOCOL_MIN: lo aceptan los kernels viejos y los nuevos
	ipsetAttrProtocol   = 1
	ipsetAttrSetname    = 2
	ipsetAttrData       = 7
	ipsetAttrIP         = 1
	ipsetAttrTimeout    = 6
	ipsetAttrIPAddrIPv4 = 1
	nlaFNested          = 0x8000
	nlaFNetByteorder    = 0x4000
	nlmFRequest         = 0x1
	nlmFAck             = 0x4
	nlmsgHdrLen         = 16
	nlmsgErrorType      = 2
)

// mensajeAddIPSet es el IPSET_CMD_ADD de ip (IPv4) al set, con su timeout si
// es > 0. Sin NLM_F_EXCL: el kernel lo trata como `-exist` (renueva en vez de
// devolver "ya existe").
func mensajeAddIPSet(seq uint32, set string, ip [4]byte, timeout int) []byte {
	le := binary.NativeEndian
	attr := func(tipo uint16, datos []byte) []byte {
		b := make([]byte, 4, 4+len(datos)+3)
		le.PutUint16(b[0:], uint16(4+len(datos)))
		le.PutUint16(b[2:], tipo)
		b = append(b, datos...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	ipAttr := attr(ipsetAttrIP|nlaFNested, attr(ipsetAttrIPAddrIPv4|nlaFNetByteorder, ip[:]))
	datos := ipAttr
	if timeout > 0 {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, uint32(timeout))
		datos = append(datos, attr(ipsetAttrTimeout|nlaFNetByteorder, t)...)
	}
	cuerpo := []byte{syscall.AF_INET, 0, 0, 0} // nfgenmsg: familia, versión 0, res_id 0
	cuerpo = append(cuerpo, attr(ipsetAttrProtocol, []byte{ipsetProtocol})...)
	cuerpo = append(cuerpo, attr(ipsetAttrSetname, append([]byte(set), 0))...)
	cuerpo = append(cuerpo, attr(ipsetAttrData|nlaFNested, datos)...)

	m := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(cuerpo))
	le.PutUint32(m[0:], uint32(nlmsgHdrLen+len(cuerpo)))
	le.PutUint16(m[4:], nfnlSubsysIPSet<<8|ipsetCmdAdd)
	le.PutUint16(m[6:], nlmFRequest|nlmFAck)
	le.PutUint32(m[8:], seq)
	return append(m, cuerpo...)
}

// errnoNetlink busca en b el NLMSG_ERROR de seq y devuelve su errno (0 es el
// ACK).
func errnoNetlink(b []byte, seq uint32) (errno int32, ok bool) {
	le := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		n := int(le.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return 0, false
		}
		if le.Uint16(b[4:]) == nlmsgErrorType && le.Uint32(b[8:]) == seq && n >= nlmsgHdrLen+4 {
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
