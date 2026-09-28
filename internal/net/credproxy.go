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
//
// Al lado, en n.HostIP:pgPort, el proxy de Postgres del mismo credproxy.Proxy
// (mismas credenciales, límites y registro): cualquier otro puerto TCP de
// n.HostIP que abra el invitado (el 5432 de un dominio con credencial
// Postgres, normalmente) llega ahí por otro DNAT.

import (
	"context"
	"errors"
	"fmt"
	"log"
	stdnet "net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// credPort es el puerto del host donde escucha el proxy de cada microVM. El
// invitado conecta al 80 de la IP que le da el resolver (n.HostIP) y un DNAT del
// netns lo lleva aquí. No es el 80 a propósito: en el host puede haber algo
// atado a 0.0.0.0:80 y el bind a n.HostIP:80 chocaría con él.
const credPort = 5380

// pgPort es el puerto del host del proxy de Postgres de cada microVM.
const pgPort = 5381

var (
	credMu      sync.Mutex
	credProxies = map[string]*credProxy{}
)

// credProxy es el proxy de un netns con el servidor que lo sirve.
type credProxy struct {
	proxy *credproxy.Proxy
	srv   *http.Server
	pg    *credproxy.PGServer
}

// SetCredentials fija el juego COMPLETO de credenciales de la máquina de n
// (sustituye el anterior): arranca su proxy si no lo tiene y avisa a su
// resolver de qué dominios debe desviar hacia él. Solo tiene sentido en modo
// allowlist, que es donde hay resolver propio. Con la lista vacía el proxy se
// queda sin credenciales (todo 403) y el resolver deja de desviar nada.
//
// auditPath es el registro de auditoría de la máquina (una línea por petición,
// ver pkg/credproxy/auditoria.go); "" = sin registro. Solo cuenta al arrancar
// el proxy: la máquina es siempre la misma, y su ruta también.
func SetCredentials(n *Net, creds []credproxy.Credential, auditPath string) error {
	if err := credproxy.ValidarCredenciales(creds); err != nil {
		return err
	}
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r == nil {
		return errors.New("credentials need -egress allowlist (the machine has no resolver of its own)")
	}
	p, err := startCredProxy(n, auditPath)
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

func startCredProxy(n *Net, auditPath string) (*credProxy, error) {
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
	lnPG, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: ip, Port: pgPort})
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("credential proxy: could not listen on %s:%d: %w", n.HostIP, pgPort, err)
	}
	p := newCredProxy(auditPath)
	p.srv = credproxy.NewServer(p.proxy)
	p.pg = credproxy.NewPGServer(p.proxy)
	go func() { _ = p.srv.Serve(ln) }()
	go func() { _ = p.pg.Serve(lnPG) }()
	credProxies[n.NS] = p
	return p, nil
}

// newCredProxy crea el proxy sin atarlo a ningún socket. Resuelve por el mismo
// resolver público que siembra el ipset, para que lo que el proxy alcanza sea
// lo mismo que el modo allowlist dejaría ver.
func newCredProxy(auditPath string) *credProxy {
	return &credProxy{proxy: credproxy.New(credproxy.Options{
		Lookup:    func(_ context.Context, host string) []string { return resolvePublicIPv4(host) },
		TempDir:   credTempDir(),
		AuditPath: auditPath,
		Logf:      log.Printf,
	})}
}

// credTempDir es $KLING_ROOT/tmp, creado con permisos 0700 si hace falta: un
// cuerpo grande con Content-Length se derrama ahí con la clave real dentro
// (ver pkg/credproxy/cuerpo.go), y ese directorio solo lo lee el daemon (el
// dueño de KLING_ROOT), a diferencia de un /tmp que comparte con cualquier
// otra cosa del host. Sin KLING_ROOT, o si no se puede crear, "": el proxy cae
// en os.TempDir() por su cuenta.
func credTempDir() string {
	root := os.Getenv("KLING_ROOT")
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

func stopCredProxy(ns string) {
	credMu.Lock()
	p := credProxies[ns]
	delete(credProxies, ns)
	credMu.Unlock()
	if p == nil {
		return
	}
	// Primero el servidor, para que no entren más peticiones; después el
	// registro, que escribe lo que quede en su cola.
	if p.srv != nil {
		_ = p.srv.Close()
	}
	if p.pg != nil {
		_ = p.pg.Close()
	}
	_ = p.proxy.Close()
}

// credPortStr y pgPortStr son los puertos en texto, para las reglas.
var (
	credPortStr = strconv.Itoa(credPort)
	pgPortStr   = strconv.Itoa(pgPort)
)
