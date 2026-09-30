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
// Al lado, en n.HostIP:pgPort, los proxies de bases de datos del mismo
// credproxy.Proxy (mismas credenciales, límites y registro): cualquier otro
// puerto TCP de n.HostIP que abra el invitado (el 5432 de un dominio con
// credencial Postgres, normalmente) llega ahí por otro DNAT, y ServeDB decide
// si habla Postgres o MySQL. El 3306 tiene su propio DNAT a n.HostIP:myPort:
// el DNAT del netns no deja ver al host el puerto original, y con
// credenciales de los dos tipos el 3306 es el que dice "MySQL" (ver
// pkg/credproxy/mysql.go).

import (
	"context"
	"errors"
	"fmt"
	"log"
	stdnet "net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// credPort es el puerto del host donde escucha el proxy de cada microVM. El
// invitado conecta al 80 de la IP que le da el resolver (n.HostIP) y un DNAT del
// netns lo lleva aquí. No es el 80 a propósito: en el host puede haber algo
// atado a 0.0.0.0:80 y el bind a n.HostIP:80 chocaría con él.
const credPort = 5380

// pgPort es el puerto del host de los proxies de bases de datos de cada
// microVM (cualquier puerto del invitado); myPort, el del 3306 del invitado.
const (
	pgPort = 5381
	myPort = 5382
)

var (
	credMu      sync.Mutex
	credProxies = map[string]*credProxy{}
)

// credProxy es el proxy de un netns con el servidor que lo sirve.
type credProxy struct {
	proxy *credproxy.Proxy
	srv   *http.Server
	pg    *credproxy.PGServer
	my    *credproxy.PGServer
	// resolve es el ResolveMachine vigente (ver SetCredentials); el proxy lo
	// consulta en cada conexión a través de resolverMaquina.
	resolve atomic.Pointer[credproxy.ResolveMachineFunc]
}

// resolverMaquina es el Options.ResolveMachine del proxy: delega en el
// resolvedor que dio la última entrega de credenciales.
func (p *credProxy) resolverMaquina(id, owner string, port int) (string, error) {
	f := p.resolve.Load()
	if f == nil || *f == nil {
		return "", errors.New("this daemon does not resolve kindling machines for this machine")
	}
	return (*f)(id, owner, port)
}

// InvalidarMaquina corta, en el proxy de cada máquina, las sesiones de
// Postgres vivas hacia la máquina id (una copia de kling db que se congela,
// se para, se borra o se reetiqueta). Devuelve cuántas cortó.
func InvalidarMaquina(id string) int {
	credMu.Lock()
	ps := make([]*credProxy, 0, len(credProxies))
	for _, p := range credProxies {
		ps = append(ps, p)
	}
	credMu.Unlock()
	n := 0
	for _, p := range ps {
		n += p.proxy.Invalidar(id)
	}
	return n
}

// InvalidarAgente corta TODAS las sesiones hacia otras máquinas del proxy de
// la máquina de n (el agente cambió de dueño, por ejemplo).
func InvalidarAgente(n *Net) int {
	credMu.Lock()
	p := credProxies[n.NS]
	credMu.Unlock()
	if p == nil {
		return 0
	}
	return p.proxy.Invalidar("")
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
//
// resolve es cómo el proxy llega a una credencial con UpstreamMachine (una
// copia de kling db en otra máquina, ver pkg/credproxy/maquina.go); lo da el
// manager, atado a ESTA máquina. Se sustituye en cada llamada; nil deja esas
// credenciales sin marcar.
func SetCredentials(n *Net, creds []credproxy.Credential, auditPath string, resolve credproxy.ResolveMachineFunc) error {
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
	p.resolve.Store(&resolve)
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
	lnMy, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: ip, Port: myPort})
	if err != nil {
		_ = ln.Close()
		_ = lnPG.Close()
		return nil, fmt.Errorf("credential proxy: could not listen on %s:%d: %w", n.HostIP, myPort, err)
	}
	p := newCredProxy(auditPath)
	p.srv = credproxy.NewServer(p.proxy)
	p.pg = credproxy.NewPGServer(p.proxy)
	p.my = credproxy.NewPGServer(p.proxy)
	p.my.DestPort = credproxy.MySQLDefaultPort
	go func() { _ = p.srv.Serve(ln) }()
	go func() { _ = p.pg.Serve(lnPG) }()
	go func() { _ = p.my.Serve(lnMy) }()
	credProxies[n.NS] = p
	return p, nil
}

// newCredProxy crea el proxy sin atarlo a ningún socket. Resuelve por el mismo
// resolver público que siembra el ipset, para que lo que el proxy alcanza sea
// lo mismo que el modo allowlist dejaría ver.
func newCredProxy(auditPath string) *credProxy {
	p := &credProxy{}
	p.proxy = credproxy.New(credproxy.Options{
		Lookup:         func(_ context.Context, host string) []string { return resolvePublicIPv4(host) },
		TempDir:        credTempDir(),
		AuditPath:      auditPath,
		Logf:           log.Printf,
		ResolveMachine: p.resolverMaquina,
	})
	return p
}

// credTmp es el directorio donde el proxy derrama un cuerpo grande con
// Content-Length (con la clave real dentro, ver pkg/credproxy/cuerpo.go). Lo
// fija el daemon con SetCredTempDir antes de levantar ninguna máquina; sin él,
// "" y el proxy no escribe nunca a disco (ese cuerpo sale chunked). Antes se
// leía de $KLING_ROOT, que el daemon recibe por -root y no por el entorno: en
// la instalación normal el fichero acababa en /tmp.
var credTmp atomic.Value // string

// SetCredTempDir prepara dir (0700, vaciado de los temporales que dejó un
// daemon muerto a mitad de una petición) y lo fija como directorio de derrame
// de los proxies que se creen a partir de ahora. Si no se puede preparar,
// devuelve el error y los proxies no derraman a disco.
func SetCredTempDir(dir string) error {
	if err := credproxy.PrepararTempDir(dir); err != nil {
		credTmp.Store("")
		return err
	}
	credTmp.Store(dir)
	return nil
}

func credTempDir() string {
	d, _ := credTmp.Load().(string)
	return d
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
	if p.my != nil {
		_ = p.my.Close()
	}
	_ = p.proxy.Close()
}

// credPortStr, pgPortStr y myPortStr son los puertos en texto, para las
// reglas.
var (
	credPortStr = strconv.Itoa(credPort)
	pgPortStr   = strconv.Itoa(pgPort)
	myPortStr   = strconv.Itoa(myPort)
)
