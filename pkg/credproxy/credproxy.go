// Package credproxy es el proxy de credenciales del modo allowlist: la parte
// que no depende de cómo llega el tráfico del invitado. La usan el daemon de
// Linux (internal/net, que lo sirve en el lado host del veth de cada netns) y
// el backend de macOS (vz/, que lo servirá sobre la pila de red de gVisor).
// Por eso vive en pkg/ y solo depende de la biblioteca estándar: el módulo vz/
// no puede importar internal/ de otro módulo.
//
// EL HUECO QUE CIERRA: con un secreto inyectado por MMDS la clave real vive
// dentro del invitado, y el invitado es HOSTIL: un servidor MCP comprometido la
// lee (corre como root) y la usa o la saca por un dominio permitido. Aislar el
// proceso no limita lo que se hace con la clave que se le entrega.
//
// EL MODELO: la clave real no entra nunca en la microVM.
//   - El invitado recibe un MARCADOR (kling-cred-...) en la variable de entorno
//     que su SDK espera. El marcador NO es un secreto: es una cadena que el
//     proxy reconoce y cambia. La capacidad de usar la clave es poder hablar con
//     este proxy, y eso solo lo permite la red de ESA máquina (quién escucha y
//     dónde lo decide el llamador, no este paquete). Por eso un marcador en un
//     snapshot no compromete nada.
//   - Para el dominio con credencial, el resolver de la máquina contesta con la
//     IP del proxy, no con la real, y la real no se permite en la salida: no
//     hay camino directo a ese dominio que esquive el proxy.
//   - El SDK habla HTTP en claro (base URL http://dominio) hasta el proxy. El
//     proxy mira el Host, cambia el marcador por la clave y sale por HTTPS
//     verificando el certificado. Sin MITM: el invitado no tiene que confiar en
//     ninguna CA nuestra.
//   - En la respuesta, cualquier aparición de la clave (una API que devuelve eco
//     de cabeceras) se sustituye por el marcador antes de llegar al invitado.
//
// DÓNDE SE SUSTITUYE el marcador: en las cabeceras (también dentro del base64
// de Authorization: Basic), en la query de la URL (APIs con ?key=) y en el
// cuerpo, en flujo y con una ventana acotada (ver sustituidor), sea del tamaño
// que sea y venga con longitud o chunked. Si el cuerpo ya sustituido cabe en
// MaxSwapBody sale con su Content-Length; si no, sale chunked (la longitud
// nueva no se sabe hasta el final).
//
// QUÉ PETICIONES se firman: todas las del dominio, salvo que la credencial
// traiga Allow (ver permisos.go). Si ninguna credencial del Host permite el
// método y la ruta, 403 antes de leer el cuerpo o abrir la salida; y una
// credencial que no la permite no se sustituye en ella aunque otra del mismo
// dominio sí.
//
// QUÉ SE REDACTA en la vuelta: la clave, sus formas escapadas más comunes
// (JSON con \/ o \u00XX, percent-encoding, entidades HTML) y cada valor de
// cabecera tal y como salió sustituido (así el eco de un Basic, que lleva la
// clave dentro del base64, también vuelve con el marcador). Una respuesta con
// una codificación que el proxy no puede inspeccionar (brotli, deflate…) no se
// entrega: 502. Es defensa en profundidad —los proveedores serios no devuelven
// la credencial— y por eso se acepta que no cubra todas las transformaciones
// imaginables.
//
// QUÉ NO RESUELVE: el invitado puede seguir USANDO la credencial contra su
// dominio (el proxy es un oráculo de ella). Eso lo acota la clave misma: de
// solo lectura, restringida o con límites de gasto en el proveedor, y Allow
// (permisos.go) cuando el proveedor no las ofrece. Lo que ya no puede es LEERLA, sacarla a otro dominio ni llevársela en un snapshot.
//
// LÍMITES frente a un invitado que dispara a saco: peticiones en vuelo,
// cabeceras y cuerpo de la petición acotados, plazos de inactividad y un techo
// absoluto por petición (plazos.go).
// No se siguen redirecciones: una 3xx del proveedor hacia otro host no debe
// llevarse la clave.
package credproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// PlaceholderPrefix es el comienzo de todo marcador.
	PlaceholderPrefix = "kling-cred-"
	// MaxCredentials: credenciales por máquina.
	MaxCredentials = 16
	// MaxSecret: tamaño de una clave.
	MaxSecret = 4096
	// MaxInFlight: peticiones a la vez por proxy antes de contestar 503.
	MaxInFlight = 32
	// MaxBody: cuerpo de una petición del invitado.
	MaxBody = 10 << 20
	// MaxSwapBody: si el cuerpo, ya sustituido, cabe en esto, se reenvía con
	// su Content-Length; si no, chunked. Es lo que se retiene por petición.
	MaxSwapBody = 1 << 20
	// MaxHeaderBytes: cabeceras de una petición del invitado.
	MaxHeaderBytes = 64 << 10
)

// Credential es una clave atada a un dominio. Secret vive solo en memoria del
// daemon (y cifrado en su disco); Placeholder es lo único que ve el invitado,
// en la variable Env. Puede haber varias para el mismo dominio: una API que
// quiera dos cabeceras distintas (clave y organización, por ejemplo).
//
// Allow acota las peticiones en que se usa ("GET /v1/balance", ver
// permisos.go); vacío es todas. El almacén cifrado del daemon guarda este
// struct tal cual (sin etiquetas: las claves JSON son los nombres), así que un
// fichero de antes de Allow se lee con Allow vacío, que es lo que hacía
// entonces.
type Credential struct {
	Env         string
	Domain      string
	Placeholder string
	Secret      string
	Allow       []string `json:",omitempty"`
}

var reDominio = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidarDominio exige un nombre de host exacto: sin comodines, sin puerto, sin
// IP. Una credencial para "*.ejemplo.com" o para una IP abriría el marcador a
// más destinos de los que el operador eligió. Devuelve el nombre normalizado
// (minúsculas, sin punto final).
func ValidarDominio(d string) (string, error) {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if net.ParseIP(d) != nil || !reDominio.MatchString(d) {
		return "", fmt.Errorf("credential domain %q must be an exact host name (no wildcard, port or IP)", d)
	}
	return d, nil
}

// NuevoMarcador genera el valor que el invitado verá en lugar de la clave. Es
// aleatorio para que no choque con nada que el invitado tuviera, no porque
// deba ser secreto (ver la documentación del paquete).
func NuevoMarcador() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return PlaceholderPrefix + hex.EncodeToString(b[:]), nil
}

// ValidarCredenciales comprueba lo que el proxy exige de un juego de
// credenciales y NORMALIZA en el sitio el Domain de cada una (por eso recibe
// el slice y no una copia). Lo usa Proxy.SetCredentials y también el manager
// antes de guardar nada.
func ValidarCredenciales(creds []Credential) error {
	if len(creds) > MaxCredentials {
		return fmt.Errorf("at most %d credentials per machine", MaxCredentials)
	}
	vistos := map[string]bool{}
	for i := range creds {
		c := &creds[i]
		d, err := ValidarDominio(c.Domain)
		if err != nil {
			return err
		}
		c.Domain = d
		if c.Secret == "" || len(c.Secret) > MaxSecret {
			return fmt.Errorf("credential for %s: the secret must be 1-%d bytes", d, MaxSecret)
		}
		if !strings.HasPrefix(c.Placeholder, PlaceholderPrefix) || len(c.Placeholder) <= len(PlaceholderPrefix) {
			return fmt.Errorf("credential for %s: invalid placeholder", d)
		}
		if vistos[c.Placeholder] {
			return fmt.Errorf("credential for %s: duplicate placeholder", d)
		}
		vistos[c.Placeholder] = true
		if err := ValidarPermisos(c.Allow); err != nil {
			return fmt.Errorf("credential for %s: %w", d, err)
		}
	}
	return nil
}

// Options configura un Proxy. El valor cero sirve: resuelve por
// PublicIPv4Lookup(DefaultDNS) y sale con el transporte seguro del paquete.
type Options struct {
	// Lookup resuelve el dominio del proveedor a sus IPv4 públicas. Conviene
	// que sea el MISMO resolver que ve el invitado en modo allowlist, para que
	// lo que el proxy alcanza coincida con lo que se permitió. Sea cual sea,
	// el dialer descarta además toda IP de IsBlockedIP: un DNS envenenado no
	// debe llevar la clave a la LAN ni a los metadatos del cloud.
	Lookup LookupFunc
	// Transport sustituye al transporte seguro. Solo para tests (un proveedor
	// de httptest en 127.0.0.1): en producción anula la barrera de IPs.
	Transport http.RoundTripper
}

// Proxy es el http.Handler del proxy de credenciales de UNA máquina. Quien lo
// sirve decide dónde (un listener TCP en el lado host del veth, un listener de
// gVisor…), y debe asegurarse de que solo esa máquina llega a él.
type Proxy struct {
	mu     sync.RWMutex
	creds  map[string][]credCompilada // por dominio
	sem    chan struct{}
	client *http.Client
	// idle y max son IdleTimeout y MaxDuration; campos para que los tests no
	// tengan que esperar minutos.
	idle, max time.Duration
}

// credCompilada es una credencial con su Allow ya partido.
type credCompilada struct {
	Credential
	reglas []regla
}

// permite dice si la credencial se usa en method sobre ruta (normalizada).
func (c credCompilada) permite(method, ruta string) bool {
	if len(c.reglas) == 0 {
		return true
	}
	for _, r := range c.reglas {
		if r.casa(method, ruta) {
			return true
		}
	}
	return false
}

// New crea un proxy sin credenciales (todo 403 hasta SetCredentials).
func New(o Options) *Proxy {
	tr := o.Transport
	if tr == nil {
		lookup := o.Lookup
		if lookup == nil {
			lookup = PublicIPv4Lookup(DefaultDNS)
		}
		tr = salidaSegura(lookup)
	}
	return &Proxy{
		creds: map[string][]credCompilada{},
		sem:   make(chan struct{}, MaxInFlight),
		// Sin Timeout: cortaría a mitad un stream largo. Los plazos van por
		// petición en ServeHTTP (ver plazos.go).
		client: &http.Client{
			Transport: tr,
			// Una redirección a otro host no debe llevarse la clave: se devuelve
			// la 3xx tal cual y que decida el invitado (con su marcador).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		idle: IdleTimeout,
		max:  MaxDuration,
	}
}

// SetCredentials fija el juego COMPLETO de credenciales (sustituye el
// anterior) tras validarlo con ValidarCredenciales, que normaliza en el sitio
// los dominios de creds. Devuelve los dominios distintos, ordenados: los que
// el resolver de la máquina debe desviar hacia el proxy. Con la lista vacía el
// proxy se queda sin credenciales (todo 403).
func (p *Proxy) SetCredentials(creds []Credential) ([]string, error) {
	if err := ValidarCredenciales(creds); err != nil {
		return nil, err
	}
	byDomain := map[string][]credCompilada{}
	for _, c := range creds {
		reglas, err := compilarPermisos(c.Allow)
		if err != nil {
			return nil, err
		}
		c.Allow = slices.Clone(c.Allow)
		byDomain[c.Domain] = append(byDomain[c.Domain], credCompilada{Credential: c, reglas: reglas})
	}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	p.mu.Lock()
	p.creds = byDomain
	p.mu.Unlock()
	return domains, nil
}

// NewServer envuelve h (normalmente un *Proxy) en un http.Server con los
// plazos y el tope de cabeceras que un invitado hostil no debe poder saltarse.
// El llamador lo sirve con srv.Serve(ln) sobre el listener que corresponda y
// lo cierra con srv.Close().
//
// Sin ReadTimeout ni WriteTimeout: son plazos TOTALES y cortarían un stream
// largo. El Proxy pone los suyos a cada petición (inactividad y techo, ver
// plazos.go); aquí quedan los de antes de que haya petición.
func NewServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// hopByHop son las cabeceras de un solo salto: no se reenvían (RFC 9110 §7.6.1).
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		rechazar(w, "kindling credential proxy: too many requests in flight", http.StatusServiceUnavailable)
		return
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	p.mu.RLock()
	todas := p.creds[host]
	p.mu.RUnlock()
	if len(todas) == 0 {
		rechazar(w, "kindling credential proxy: no credential for "+host, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		rechazar(w, "kindling credential proxy: CONNECT is not supported; use http://"+host, http.StatusMethodNotAllowed)
		return
	}

	// Permisos: solo se usan las credenciales que permiten este método y esta
	// ruta. Si ninguna, 403 aquí: sin leer el cuerpo ni abrir la salida.
	restringido := false
	for _, c := range todas {
		if len(c.reglas) > 0 {
			restringido = true
			break
		}
	}
	// Con Allow de por medio, antes de normalizar y comparar: una ruta CRUDA
	// ambigua (ver rutaAmbigua) se rechaza aquí, sin decidir si "casaría" tras
	// limpiarla. Sin ninguna credencial con Allow no se mira: el invitado manda
	// lo que quiera, como antes de que existiera Allow.
	if restringido {
		if motivo := rutaAmbigua(r.URL); motivo != "" {
			rechazar(w, "kindling credential proxy: ambiguous path ("+motivo+") for "+host, http.StatusForbidden)
			return
		}
	}
	ruta := rutaNormalizada(r.URL)
	var cs []Credential
	for _, c := range todas {
		if c.permite(r.Method, ruta) {
			cs = append(cs, c.Credential)
		}
	}
	if len(cs) == 0 {
		rechazar(w, "kindling credential proxy: "+r.Method+" "+ruta+" is not allowed for "+host, http.StatusForbidden)
		return
	}

	// Un Content-Length por encima del tope se rechaza ya: con MaxBytesReader la
	// subida se cortaría a medias y la petición saliente se quedaría esperando
	// bytes que no llegan hasta el plazo.
	if r.ContentLength > MaxBody {
		rechazar(w, "kindling credential proxy: request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Plazos (plazos.go): techo absoluto en el contexto, vigía de inactividad
	// que lo cancela, y los plazos de la conexión del invitado renovados en
	// cada movimiento. Al salir se quita el de escritura: el servidor no lo
	// repone y se lo llevaría la siguiente petición de la misma conexión.
	ctx, cancel := context.WithTimeout(r.Context(), p.max)
	defer cancel()
	v := nuevoVigia(p.idle, cancel)
	defer v.parar()
	pl := plazosInvitado{rc: http.NewResponseController(w), idle: p.idle, fin: time.Now().Add(p.max)}
	defer func() { _ = pl.rc.SetWriteDeadline(time.Time{}) }()

	// La respuesta se redacta con TODAS las credenciales del dominio, también
	// las que no se usaron: defensa en profundidad, no cuesta nada.
	red := nuevoRedactor(escritorVigilado{w: w, v: v, p: pl}, credencialesDe(todas))

	body, length, err := cuerpoSaliente(r, w, cs, v, pl)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "kindling credential proxy: request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "kindling credential proxy: could not read the request body", http.StatusBadRequest)
		return
	}

	// Con permisos en el dominio, al proveedor va la ruta que se comprobó (ver
	// urlSaliente); sin ellos, la del invitado tal cual, como siempre.
	u := r.URL
	if restringido {
		u = urlSaliente(r.URL)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, "https://"+host+sustituirQuery(u, cs), body)
	if err != nil {
		http.Error(w, "kindling credential proxy: bad request", http.StatusBadRequest)
		return
	}
	out.ContentLength = length
	for k, vs := range r.Header {
		for _, v := range vs {
			nv, pares := sustituir(k, v, cs)
			// El valor tal y como sale, para redactarlo si el proveedor lo
			// devuelve en eco: cubre el base64 de un Basic y cualquier otra
			// forma en que el invitado hubiera envuelto el marcador.
			if nv != v {
				red.par(nv, v)
			}
			for _, pr := range pares {
				red.par(pr[0], pr[1])
			}
			out.Header.Add(k, nv)
		}
	}
	for _, h := range hopByHop {
		out.Header.Del(h)
	}
	out.Header.Del("X-Forwarded-For")
	// NO se reenvía Accept-Encoding a propósito: con él, el proveedor contestaría
	// comprimido y una clave en eco viajaría dentro del gzip sin que el redactor
	// la viera. Sin él, el transporte de Go pide gzip por su cuenta y lo
	// descomprime antes de que el cuerpo pase por el redactor.
	out.Header.Del("Accept-Encoding")
	out.Host = host

	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "kindling credential proxy: upstream error: "+red.texto(err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// Go quita Content-Encoding cuando descomprime el gzip que pidió él. Si
	// queda otra codificación, el proveedor la impuso sin que nadie se la
	// pidiera y el redactor no vería una clave dentro: no se entrega.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		http.Error(w, "kindling credential proxy: upstream replied with Content-Encoding "+enc+
			", which the proxy can't inspect for the key", http.StatusBadGateway)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, red.texto(v))
		}
	}
	for _, h := range hopByHop {
		w.Header().Del(h)
	}
	// La longitud puede cambiar al redactar: que la calcule el servidor.
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(red, origenVigilado{r: resp.Body, v: v}); err != nil {
		// Cortada a medias (inactividad, techo, el proveedor se fue): se aborta
		// la conexión en vez de cerrar el chunked limpio, para que el SDK del
		// invitado vea un error y no una respuesta truncada que parece entera.
		panic(http.ErrAbortHandler)
	}
}

// rechazar contesta un error ANTES de leer el cuerpo y cierra la conexión. Sin
// el cierre, el servidor de net/http lee y descarta hasta 256 KiB del cuerpo
// antes de mandar la respuesta, para poder reutilizar la conexión: el 403
// esperaría a la subida que se quería no aceptar.
func rechazar(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Connection", "close")
	http.Error(w, msg, code)
}

func credencialesDe(cc []credCompilada) []Credential {
	out := make([]Credential, len(cc))
	for i, c := range cc {
		out[i] = c.Credential
	}
	return out
}

// cuerpoSaliente prepara el cuerpo que va al proveedor: el del invitado
// (acotado a MaxBody y vigilado) con el marcador cambiado por la clave en
// flujo. Lee por adelantado hasta MaxSwapBody de salida: si el cuerpo termina
// antes, sale entero con su Content-Length (un formulario OAuth, un JSON
// normal: hay servidores que no aceptan una subida chunked); si no, lo leído
// va delante del resto y sale chunked (longitud -1). Eso es lo único que se
// retiene por petición, lo mismo que antes de sustituir en flujo.
func cuerpoSaliente(r *http.Request, w http.ResponseWriter, cs []Credential, v *vigia, pl plazosInvitado) (io.Reader, int64, error) {
	if r.ContentLength == 0 {
		return nil, 0, nil
	}
	s := nuevoSustituidor(lectorVigilado{r: http.MaxBytesReader(w, r.Body, MaxBody), v: v, p: pl}, cs)
	// A mano y no con io.ReadAll: su crecimiento al doble reservaría hasta 2 MiB
	// para retener 1. Aquí la capacidad no pasa de MaxSwapBody+1.
	const tope = MaxSwapBody + 1
	hint := 32 << 10
	if r.ContentLength > 0 {
		hint = int(min(r.ContentLength+1024, tope))
	}
	buf := make([]byte, 0, hint)
	for len(buf) < tope {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(cap(buf), tope-len(buf)))
		}
		n, err := s.Read(buf[len(buf):min(cap(buf), tope)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return bytes.NewReader(buf), int64(len(buf)), nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return io.MultiReader(bytes.NewReader(buf), s), -1, nil
}
