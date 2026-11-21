package net

import (
	"encoding/binary"
	"fmt"
	stdnet "net"
	"syscall"
)

// volcarDirecciones pide al kernel todas las direcciones IPv4 del host en un
// volcado RTM_GETADDR. Variable para que los tests cuenten los volcados.
var volcarDirecciones = func() ([]byte, error) {
	return syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_INET)
}

// volcadosMax: intentos de un volcado antes de rendirse si sale interrumpido.
const volcadosMax = 3

// nlmFDumpIntr es NLM_F_DUMP_INTR: el kernel avisa de que el volcado no es
// coherente.
const nlmFDumpIntr = 0x10

func volcadoInterrumpido(msgs []syscall.NetlinkMessage) bool {
	for i := range msgs {
		if msgs[i].Header.Flags&nlmFDumpIntr != 0 {
			return true
		}
	}
	return false
}

// direccionesHost es DireccionesHost en Linux: un volcado de interfaces (para
// los nombres) y uno de direcciones, sea cual sea el número de interfaces.
// Lee la dirección como la biblioteca estándar: IFA_LOCAL si la hay (en un
// enlace punto a punto IFA_ADDRESS es la del otro extremo), si no
// IFA_ADDRESS.
func direccionesHost() ([]DireccionHost, error) {
	// NetlinkRIB no mira NLM_F_DUMP_INTR: si las direcciones cambiaron durante
	// el volcado (interfaces naciendo y muriendo), el kernel lo marca y puede
	// faltar alguna. Un volcado así se repite; al tercero, error.
	// Direcciones primero y nombres después: una interfaz que aparece entre los
	// dos volcados tiene su nombre. Si aun así falta alguno, se relee una vez.
	var msgs []syscall.NetlinkMessage
	for intento := 1; ; intento++ {
		tab, err := volcarDirecciones()
		if err != nil {
			return nil, err
		}
		msgs, err = syscall.ParseNetlinkMessage(tab)
		if err != nil {
			return nil, err
		}
		if !volcadoInterrumpido(msgs) {
			break
		}
		if intento == volcadosMax {
			return nil, fmt.Errorf("the address dump was interrupted (NLM_F_DUMP_INTR) %d times in a row: the host's addresses change too fast to list them", volcadosMax)
		}
	}
	nombres, err := nombresInterfaces()
	if err != nil {
		return nil, err
	}
	reintentado := false
	var res []DireccionHost
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Type == syscall.NLMSG_DONE {
			break
		}
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < syscall.SizeofIfAddrmsg ||
			m.Data[0] != syscall.AF_INET {
			continue
		}
		// IfAddrmsg: familia, prefijo, flags y ámbito (un byte cada uno) y el
		// índice de la interfaz, en el orden de bytes de la máquina.
		idx := binary.NativeEndian.Uint32(m.Data[4:8])
		nombre, ok := nombres[idx]
		if !ok && !reintentado {
			reintentado = true
			if n2, err := nombresInterfaces(); err == nil {
				nombres = n2
				nombre, ok = nombres[idx]
			}
		}
		if !ok {
			continue // la interfaz se fue entre los dos volcados
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(m)
		if err != nil {
			continue
		}
		var local, dir stdnet.IP
		for _, a := range attrs {
			if len(a.Value) < 4 {
				continue
			}
			switch a.Attr.Type {
			case syscall.IFA_LOCAL:
				local = stdnet.IPv4(a.Value[0], a.Value[1], a.Value[2], a.Value[3]).To4()
			case syscall.IFA_ADDRESS:
				dir = stdnet.IPv4(a.Value[0], a.Value[1], a.Value[2], a.Value[3]).To4()
			}
		}
		if local != nil {
			dir = local
		}
		if dir != nil {
			res = append(res, DireccionHost{If: nombre, IP: dir})
		}
	}
	return res, nil
}

func nombresInterfaces() (map[uint32]string, error) {
	ifs, err := stdnet.Interfaces()
	if err != nil {
		return nil, err
	}
	nombres := make(map[uint32]string, len(ifs))
	for _, it := range ifs {
		nombres[uint32(it.Index)] = it.Name
	}
	return nombres, nil
}
