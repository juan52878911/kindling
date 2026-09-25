// Package net monta la red de cada microVM.
//
// EL PROBLEMA: un snapshot graba el nombre del dispositivo TAP del host. Si N
// instancias restauran del mismo snapshot dorado, las N piden el mismo TAP y
// chocan. Tampoco vale reasignarlo: Firecracker no permite parchear
// host_dev_name en una interfaz de red.
//
// LA SOLUCIÓN: un namespace de red por microVM. Dentro de cada uno el TAP se
// llama SIEMPRE tap0 y el invitado tiene SIEMPRE la misma IP, así que el
// snapshot es idéntico para todas. Toda la diferenciación ocurre en el host, al
// otro lado de un veth. Es el mismo enfoque que usa AWS Lambda.
//
//	       host                    │  netns kl-<id>        │  microVM
//	vh-<id> 172.30.a.b/30  ◄─veth─►│ vg-<id> 172.30.a.b+1  │
//	                               │ tap0    172.16.0.1/30 ├─ eth0 172.16.0.2
package net

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	// El invitado ve siempre esta configuración, sea cual sea la instancia. Es
	// lo que hace que un snapshot dorado sirva para todas.
	GuestIP  = "172.16.0.2"
	GuestGW  = "172.16.0.1"
	GuestNM  = "255.255.255.252"
	GuestMAC = "06:00:AC:10:00:02"
	// TapMAC es la MAC del tap0 del lado del host, la que el invitado tiene
	// en su caché ARP para GuestGW. Fija por la misma razón que GuestMAC: la
	// caché ARP viaja congelada en la memoria del snapshot, y con una MAC
	// aleatoria por namespace cada restauración (o cada red rehecha tras un
	// thaw) despertaba con una entrada que apuntaba a un dispositivo que ya
	// no existe. Cada tap0 vive en su propio namespace: repetirla no choca.
	TapMAC  = "06:00:AC:10:00:01"
	TapName = "tap0"

	// Rango del host para los enlaces punto a punto con cada namespace.
	hostPrefix = "172.30"
)

// BootArg devuelve la configuración de red que el kernel del invitado aplica
// solo, sin necesitar herramientas dentro de la imagen.
func BootArg() string {
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", GuestIP, GuestGW, GuestNM)
}

// Net es la red de una microVM concreta.
type Net struct {
	NS     string // namespace
	HostIf string // veth del lado host
	NSIf   string // veth del lado namespace
	HostIP string // IP del host en el enlace
	NSIP   string // IP del namespace en el enlace; por aquí se alcanza la microVM
	Index  int
}

func run(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// quiet ejecuta ignorando el error; para limpiezas donde "no existe" es válido.
func quiet(args ...string) { _ = exec.Command(args[0], args[1:]...).Run() }

// exec_ok indica si un comando termina bien; se usa para comprobar reglas ya puestas.
func exec_ok(args []string) bool {
	return exec.Command(args[0], args[1:]...).Run() == nil
}

// Plan calcula el direccionamiento de una microVM a partir de su índice.
// Cada máquina consume una /30: 16384 máquinas en 172.30.0.0/16.
func Plan(index int, id string) *Net {
	third := index / 64
	fourth := (index % 64) * 4
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return &Net{
		NS:     "kl-" + short,
		HostIf: "vh-" + short,
		NSIf:   "vg-" + short,
		HostIP: fmt.Sprintf("%s.%d.%d", hostPrefix, third, fourth+1),
		NSIP:   nsIP(third, fourth),
		Index:  index,
	}
}
