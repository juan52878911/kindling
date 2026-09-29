package net

// LAS SUBREDES SON DEL HOST, NO DEL DAEMON.
//
// Cada microVM consume una /30 de 172.30.0.0/16 en el lado host de su veth, y
// el índice (Net.Index) lo reparte cada daemon mirando solo SUS máquinas. Dos
// daemons en el mismo host (el del sistema y uno privado con otra -root)
// repartían así los mismos índices: dos veth con la misma 172.30.a.b/30 en el
// host, y la red de las máquinas de ambos rota (la ruta de la /30 lleva a uno
// solo de los dos).
//
// Así que un índice solo se da por libre si además lo está en el HOST:
//
//  1. Se reserva con un fichero en DirReservas (/run, compartido por todos los
//     daemons del host y vacío tras reiniciar), creado con O_EXCL. Dos daemons
//     que eligen el mismo índice a la vez: solo uno lo crea.
//  2. DESPUÉS de reservarlo, se mira que ninguna dirección del host caiga en su
//     /30 (el veth de otro daemon que ya la montó).
//  3. La reserva se suelta cuando Setup ya puso la dirección al veth
//     (Manager.montarRed): desde ahí es la propia dirección la que la ocupa.
//
// El orden importa: quien crea la reserva después de que otro la soltara ve ya
// la dirección del otro, porque el otro la puso antes de soltarla. Mirar las
// direcciones antes de reservar dejaría una ventana entre las dos cosas.
//
// Lo que esto no ve: la red de una máquina congelada de otro daemon que se
// soltó (redDormidaMax, reinicio del daemon). Su índice parece libre y otro
// daemon lo puede tomar; al descongelarla, su daemon lo ve ocupado y le da otro
// (ver Manager.Thaw). El invitado no nota el cambio: su IP es siempre GuestIP.
//
// En macOS la red es de espacio de usuario dentro de cada kling-vz y no hay
// nada en el host que pueda chocar: Reservar siempre vale.

import (
	"errors"
	"fmt"
	"log"
	stdnet "net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// DirReservas es donde los daemons del host apuntan los índices que están
// montando. Variable para los tests.
var DirReservas = "/run/kindling/net-claims"

// reservaCaduca: una reserva más vieja es de un daemon que murió entre
// reservar y montar (montar son milisegundos).
const reservaCaduca = time.Minute

// DireccionHost es una dirección IPv4 de una interfaz del host.
type DireccionHost struct {
	If string
	IP stdnet.IP
}

// DireccionesHost lista las direcciones IPv4 de las interfaces del host.
// Variable para que los tests simulen un host compartido por dos daemons.
var DireccionesHost = direccionesHost

func direccionesHost() ([]DireccionHost, error) {
	ifs, err := stdnet.Interfaces()
	if err != nil {
		return nil, err
	}
	var res []DireccionHost
	for _, it := range ifs {
		addrs, err := it.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*stdnet.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil {
					res = append(res, DireccionHost{If: it.Name, IP: ip4})
				}
			}
		}
	}
	return res, nil
}

// IndiceDe devuelve el índice cuya /30 contiene ip, o 0 si ip no cae en el
// rango de los enlaces (172.30.0.0/16).
func IndiceDe(ip stdnet.IP) int {
	ip4 := ip.To4()
	if ip4 == nil || ip4[0] != 172 || ip4[1] != 30 {
		return 0
	}
	return int(ip4[2])*64 + int(ip4[3])/4
}

// OcupadosEnHost devuelve los índices cuya /30 tiene alguna dirección en el
// host, salvo las de la interfaz propia (el veth de la misma máquina, que Setup
// rehace).
func OcupadosEnHost(dirs []DireccionHost, propia string) map[int]bool {
	res := map[int]bool{}
	for _, d := range dirs {
		if d.If == propia {
			continue
		}
		if i := IndiceDe(d.IP); i > 0 {
			res[i] = true
		}
	}
	return res
}

var avisoReservas sync.Once

// Reservar aparta el índice de n para este daemon mientras monta su red. false
// si otro daemon lo está montando o ya lo usa en el host. La reserva se suelta
// con SoltarReserva, en cuanto Setup termina.
func (n *Net) Reservar() bool {
	if !redEnHost {
		return true
	}
	if n.Index <= 0 {
		return false
	}
	if !n.reservarFichero() {
		return false
	}
	dirs, err := DireccionesHost()
	if err != nil {
		// Sin poder mirar el host, lo de siempre: el índice de este daemon.
		return true
	}
	if OcupadosEnHost(dirs, n.HostIf)[n.Index] {
		n.SoltarReserva()
		return false
	}
	return true
}

// reservarFichero crea la reserva en DirReservas. false si otro daemon la
// tiene. Si el directorio no se puede usar, sigue sin fichero (y lo dice una
// vez): queda solo la comprobación de las direcciones del host.
func (n *Net) reservarFichero() bool {
	aviso := func(err error) {
		avisoReservas.Do(func() {
			log.Printf("warning: cannot record network claims in %s (%v): "+
				"only the host addresses protect against another daemon's subnets", DirReservas, err)
		})
	}
	if err := os.MkdirAll(DirReservas, 0o755); err != nil {
		aviso(err)
		return true
	}
	path := filepath.Join(DirReservas, strconv.Itoa(n.Index))
	for intento := 0; intento < 3; intento++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), n.NS)
			f.Close()
			n.reserva = path
			return true
		}
		if !errors.Is(err, os.ErrExist) {
			aviso(err)
			return true
		}
		fi, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // la soltaron entre medias
		case err == nil && time.Since(fi.ModTime()) > reservaCaduca:
			_ = os.Remove(path) // de un daemon que murió a medias
			continue
		}
		return false // otro daemon la está montando ahora
	}
	return false
}

// SoltarReserva suelta la reserva de Reservar, si la hay. Idempotente.
func (n *Net) SoltarReserva() {
	if n.reserva == "" {
		return
	}
	_ = os.Remove(n.reserva)
	n.reserva = ""
}

// Asignar busca el siguiente índice libre en rotación a partir de *cursor, de
// 1 a espacio: libre para este daemon (usado dice los de sus máquinas) y en el
// host (Reservar). Devuelve la red con la reserva hecha; quien la monte la
// suelta. cursor lo protege el llamante.
func Asignar(cursor *int, espacio int, usado func(int) bool, id string) (*Net, error) {
	var ocupados map[int]bool
	if redEnHost {
		// Una foto para saltarse sin reservar lo que ya se ve ocupado; la
		// comprobación que vale es la de Reservar, hecha tras reservar.
		if dirs, err := DireccionesHost(); err == nil {
			ocupados = OcupadosEnHost(dirs, "")
		}
	}
	for i := 0; i < espacio; i++ {
		*cursor = *cursor%espacio + 1
		idx := *cursor
		if usado(idx) || ocupados[idx] {
			continue
		}
		n := Plan(idx, id)
		if n.Reservar() {
			return n, nil
		}
	}
	return nil, fmt.Errorf("no free subnet for the machine's network: all %d /30 in %s.0.0/16 are in use on this host", espacio, hostPrefix)
}
