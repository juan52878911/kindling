package net

// Proxy de credenciales del modo allowlist: la parte atada a Linux. La lógica
// del proxy (sustitución del marcador, redacción, límites, salida segura) vive
// en pkg/credproxy, que comparte el backend vz; aquí solo queda dónde escucha
// y cómo se entera el resolver de la máquina.
//
// DÓNDE: en el lado host del veth de cada netns (n.HostIP:credPort). El
// invitado conecta al 80/443 de la IP que le da su resolver y un DNAT del netns
// lo trae aquí (firewall.go). El FORWARD de otros netns no llega a esa IP, así
// que solo la máquina dueña de las credenciales puede usarlas.

import (
	"context"
	"errors"
	"fmt"
	stdnet "net"
	"net/http"
	"strconv"
	"sync"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// credPort es el puerto del host donde escucha el proxy de cada microVM. El
// invitado conecta al 80 de la IP que le da el resolver (n.HostIP) y un DNAT del
// netns lo lleva aquí. No es el 80 a propósito: en el host puede haber algo
// atado a 0.0.0.0:80 y el bind a n.HostIP:80 chocaría con él.
const credPort = 5380

var (
	credMu      sync.Mutex
	credProxies = map[string]*credProxy{}
)

// credProxy es el proxy de un netns con el servidor que lo sirve.
type credProxy struct {
	proxy *credproxy.Proxy
	srv   *http.Server
}

// SetCredentials fija el juego COMPLETO de credenciales de la máquina de n
// (sustituye el anterior): arranca su proxy si no lo tiene y avisa a su
// resolver de qué dominios debe desviar hacia él. Solo tiene sentido en modo
// allowlist, que es donde hay resolver propio. Con la lista vacía el proxy se
// queda sin credenciales (todo 403) y el resolver deja de desviar nada.
func SetCredentials(n *Net, creds []credproxy.Credential) error {
	if err := credproxy.ValidarCredenciales(creds); err != nil {
		return err
	}
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r == nil {
		return errors.New("credentials need -egress allowlist (the machine has no resolver of its own)")
	}
	p, err := startCredProxy(n)
	if err != nil {
		return err
	}
	domains, err := p.proxy.SetCredentials(creds)
	if err != nil {
		return err
	}
	r.setCredHosts(domains, stdnet.ParseIP(n.HostIP))
	return nil
}

func startCredProxy(n *Net) (*credProxy, error) {
	credMu.Lock()
	defer credMu.Unlock()
	if p, ok := credProxies[n.NS]; ok {
		return p, nil
	}
	ip := stdnet.ParseIP(n.HostIP)
	if ip == nil {
		return nil, fmt.Errorf("credential proxy: invalid host IP %q", n.HostIP)
	}
	ln, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: ip, Port: credPort})
	if err != nil {
		return nil, fmt.Errorf("credential proxy: could not listen on %s:%d: %w", n.HostIP, credPort, err)
	}
	p := newCredProxy()
	p.srv = credproxy.NewServer(p.proxy)
	go func() { _ = p.srv.Serve(ln) }()
	credProxies[n.NS] = p
	return p, nil
}

// newCredProxy crea el proxy sin atarlo a ningún socket. Resuelve por el mismo
// resolver público que siembra el ipset, para que lo que el proxy alcanza sea
// lo mismo que el modo allowlist dejaría ver.
func newCredProxy() *credProxy {
	return &credProxy{proxy: credproxy.New(credproxy.Options{
		Lookup: func(_ context.Context, host string) []string { return resolvePublicIPv4(host) },
	})}
}

func stopCredProxy(ns string) {
	credMu.Lock()
	p := credProxies[ns]
	delete(credProxies, ns)
	credMu.Unlock()
	if p != nil && p.srv != nil {
		_ = p.srv.Close()
	}
}

// credPortStr es credPort en texto, para las reglas.
var credPortStr = strconv.Itoa(credPort)
