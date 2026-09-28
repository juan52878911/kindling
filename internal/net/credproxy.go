package net

// Proxy de credenciales del modo allowlist.
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
//     este proxy, y solo se puede desde dentro del netns de ESTA máquina (el
//     proxy escucha en el lado host de su veth y el FORWARD de otros netns no
//     llega). Por eso un marcador en un snapshot no compromete nada.
//   - Para el dominio con credencial, el resolver de la máquina (dnsresolver.go)
//     contesta con la IP del lado host del veth, no con la real. La IP real no
//     se siembra en el ipset, así que no hay camino directo a ese dominio que
//     esquive el proxy.
//   - El SDK habla HTTP en claro (base URL http://dominio) hasta el proxy, por
//     el veth local de la máquina. El proxy mira el Host, cambia el marcador por
//     la clave y sale por HTTPS verificando el certificado. Sin MITM: el
//     invitado no tiene que confiar en ninguna CA nuestra.
//   - En la respuesta, cualquier aparición de la clave (una API que devuelve eco
//     de cabeceras) se sustituye por el marcador antes de llegar al invitado.
//
// DÓNDE SE SUSTITUYE el marcador: en las cabeceras (también dentro del base64
// de Authorization: Basic), en la query de la URL (APIs con ?key=) y en el
// cuerpo si es pequeño (≤ credMaxSwapBody: los client_secret de OAuth van en un
// formulario de unos bytes). Un cuerpo grande o sin longitud se reenvía tal
// cual: sustituir en él obligaría a retenerlo entero en memoria por petición.
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
// solo lectura, restringida o con límites de gasto en el proveedor. Lo que ya
// no puede es LEERLA, sacarla a otro dominio ni llevársela en un snapshot.
//
// LÍMITES frente a un invitado que dispara a saco: peticiones en vuelo,
// cabeceras y cuerpo de la petición acotados, plazos por petición. No se siguen
// redirecciones: una 3xx del proveedor hacia otro host no debe llevarse la clave.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	stdnet "net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// credPort es el puerto del host donde escucha el proxy de cada microVM. El
// invitado conecta al 80 de la IP que le da el resolver (n.HostIP) y un DNAT del
// netns lo lleva aquí. No es el 80 a propósito: en el host puede haber algo
// atado a 0.0.0.0:80 y el bind a n.HostIP:80 chocaría con él.
const credPort = 5380

const (
	// credMaxInFlight: peticiones a la vez por máquina antes de contestar 503.
	credMaxInFlight = 32
	// credMaxBody: cuerpo de una petición del invitado.
	credMaxBody = 10 << 20
	// credMaxSwapBody: hasta este tamaño el cuerpo se lee entero para cambiar
	// el marcador en él; por encima se reenvía en flujo y sin tocar.
	credMaxSwapBody = 1 << 20
	// credMaxHeader: cabeceras de una petición del invitado.
	credMaxHeader = 64 << 10
	// credTimeout: una petición entera, subida y respuesta incluidas.
	credTimeout = 120 * time.Second
	// CredMax: credenciales por máquina.
	CredMax = 16
	// credMaxSecret: tamaño de una clave.
	credMaxSecret = 4096
	// PlaceholderPrefix es el comienzo de todo marcador.
	PlaceholderPrefix = "kling-cred-"
)

// Credential es una clave atada a un dominio. Secret vive solo en memoria del
// daemon (y cifrado en su disco); Placeholder es lo único que ve el invitado,
// en la variable Env. Puede haber varias para el mismo dominio: una API que
// quiera dos cabeceras distintas (clave y organización, por ejemplo).
type Credential struct {
	Env         string
	Domain      string
	Placeholder string
	Secret      string
}

var reDominio = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidarDominioCredencial exige un nombre de host exacto: sin comodines, sin
// puerto, sin IP. Una credencial para "*.ejemplo.com" o para una IP abriría el
// marcador a más destinos de los que el operador eligió.
func ValidarDominioCredencial(d string) (string, error) {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if stdnet.ParseIP(d) != nil || !reDominio.MatchString(d) {
		return "", fmt.Errorf("credential domain %q must be an exact host name (no wildcard, port or IP)", d)
	}
	return d, nil
}

// NuevoMarcador genera el valor que el invitado verá en lugar de la clave. Es
// aleatorio para que no choque con nada que el invitado tuviera, no porque
// deba ser secreto (ver la cabecera del fichero).
func NuevoMarcador() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return PlaceholderPrefix + hex.EncodeToString(b[:]), nil
}

// ValidarCredenciales comprueba lo que el proxy exige de un juego de
// credenciales y devuelve los dominios normalizados. Lo usa SetCredentials y
// también el manager antes de guardar nada.
func ValidarCredenciales(creds []Credential) error {
	if len(creds) > CredMax {
		return fmt.Errorf("at most %d credentials per machine", CredMax)
	}
	vistos := map[string]bool{}
	for i := range creds {
		c := &creds[i]
		d, err := ValidarDominioCredencial(c.Domain)
		if err != nil {
			return err
		}
		c.Domain = d
		if c.Secret == "" || len(c.Secret) > credMaxSecret {
			return fmt.Errorf("credential for %s: the secret must be 1-%d bytes", d, credMaxSecret)
		}
		if !strings.HasPrefix(c.Placeholder, PlaceholderPrefix) || len(c.Placeholder) <= len(PlaceholderPrefix) {
			return fmt.Errorf("credential for %s: invalid placeholder", d)
		}
		if vistos[c.Placeholder] {
			return fmt.Errorf("credential for %s: duplicate placeholder", d)
		}
		vistos[c.Placeholder] = true
	}
	return nil
}

var (
	credMu      sync.Mutex
	credProxies = map[string]*credProxy{}
)

type credProxy struct {
	ns     string
	mu     sync.RWMutex
	creds  map[string][]Credential // por dominio
	ln     stdnet.Listener
	srv    *http.Server
	sem    chan struct{}
	client *http.Client
}

// SetCredentials fija el juego COMPLETO de credenciales de la máquina de n
// (sustituye el anterior): arranca su proxy si no lo tiene y avisa a su
// resolver de qué dominios debe desviar hacia él. Solo tiene sentido en modo
// allowlist, que es donde hay resolver propio. Con la lista vacía el proxy se
// queda sin credenciales (todo 403) y el resolver deja de desviar nada.
func SetCredentials(n *Net, creds []Credential) error {
	if err := ValidarCredenciales(creds); err != nil {
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
	byDomain := map[string][]Credential{}
	for _, c := range creds {
		byDomain[c.Domain] = append(byDomain[c.Domain], c)
	}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	p.mu.Lock()
	p.creds = byDomain
	p.mu.Unlock()
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
	p := newCredProxy(n.NS)
	p.ln = ln
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       credTimeout,
		WriteTimeout:      credTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    credMaxHeader,
	}
	go func() { _ = p.srv.Serve(ln) }()
	credProxies[n.NS] = p
	return p, nil
}

// newCredProxy crea el proxy sin atarlo a ningún socket (los tests lo sirven con
// httptest).
func newCredProxy(ns string) *credProxy {
	return &credProxy{
		ns:    ns,
		creds: map[string][]Credential{},
		sem:   make(chan struct{}, credMaxInFlight),
		client: &http.Client{
			Transport: salidaSegura(),
			// Una redirección a otro host no debe llevarse la clave: se devuelve
			// la 3xx tal cual y que decida el invitado (con su marcador).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       credTimeout,
		},
	}
}

// salidaSegura es el transporte hacia el proveedor: TLS con las raíces del
// sistema y un dialer que resuelve por el mismo resolver público que el modo
// allowlist y se niega a conectar a una IP privada (un DNS envenenado no debe
// llevar la clave a la LAN ni a los metadatos del cloud).
func salidaSegura() *http.Transport {
	d := &stdnet.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (stdnet.Conn, error) {
			host, port, err := stdnet.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips := resolvePublicIPv4(host)
			if len(ips) == 0 {
				return nil, fmt.Errorf("credential proxy: %s does not resolve to a public IPv4", host)
			}
			var last error
			for _, ip := range ips {
				c, err := d.DialContext(ctx, "tcp4", stdnet.JoinHostPort(ip, port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
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

// hopByHop son las cabeceras de un solo salto: no se reenvían (RFC 9110 §7.6.1).
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *credProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		http.Error(w, "kindling credential proxy: too many requests in flight", http.StatusServiceUnavailable)
		return
	}
	host := strings.ToLower(r.Host)
	if h, _, err := stdnet.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	p.mu.RLock()
	cs := p.creds[host]
	p.mu.RUnlock()
	if len(cs) == 0 {
		http.Error(w, "kindling credential proxy: no credential for "+host, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		http.Error(w, "kindling credential proxy: CONNECT is not supported; use http://"+host, http.StatusMethodNotAllowed)
		return
	}

	// Un Content-Length por encima del tope se rechaza ya: con MaxBytesReader la
	// subida se cortaría a medias y la petición saliente se quedaría esperando
	// bytes que no llegan hasta el plazo entero.
	if r.ContentLength > credMaxBody {
		http.Error(w, "kindling credential proxy: request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	red := nuevoRedactor(w, cs)

	// Cuerpo: si es pequeño y declara su longitud, se lee entero y se cambia el
	// marcador en él (client_secret de OAuth, JSON con la clave dentro). Si no,
	// se reenvía en flujo tal cual.
	var body io.Reader = http.MaxBytesReader(w, r.Body, credMaxBody)
	length := r.ContentLength
	if length > 0 && length <= credMaxSwapBody {
		b, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, "kindling credential proxy: could not read the request body", http.StatusBadRequest)
			return
		}
		for _, c := range cs {
			b = bytes.ReplaceAll(b, []byte(c.Placeholder), []byte(c.Secret))
		}
		body, length = bytes.NewReader(b), int64(len(b))
	}

	ctx, cancel := context.WithTimeout(r.Context(), credTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, r.Method, "https://"+host+sustituirQuery(r.URL, cs), body)
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
	_, _ = io.Copy(red, resp.Body)
}

// sustituirQuery devuelve path+query con el marcador cambiado por la clave en
// la query (APIs que la piden como ?key=). La clave va percent-encoded, que es
// como iría si el SDK la hubiera puesto él.
func sustituirQuery(u *url.URL, cs []Credential) string {
	uri := u.RequestURI()
	if u.RawQuery == "" {
		return uri
	}
	q := u.RawQuery
	for _, c := range cs {
		q = strings.ReplaceAll(q, c.Placeholder, url.QueryEscape(c.Secret))
	}
	if q == u.RawQuery {
		return uri
	}
	v := *u
	v.RawQuery = q
	return v.RequestURI()
}

// sustituir cambia el marcador por la clave en una cabecera. Authorization:
// Basic lleva usuario:clave en base64, así que el marcador no aparece en claro:
// se decodifica, se sustituye y se vuelve a codificar; devuelve además el par
// (base64 con clave, base64 con marcador) para que el redactor lo reconozca
// aunque el eco venga sin el prefijo "Basic ".
func sustituir(k, v string, cs []Credential) (string, [][2]string) {
	if strings.EqualFold(k, "Authorization") && len(v) > 6 && strings.EqualFold(v[:6], "basic ") {
		enc := strings.TrimSpace(v[6:])
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
			cambiado := false
			for _, c := range cs {
				if bytes.Contains(raw, []byte(c.Placeholder)) {
					raw = bytes.ReplaceAll(raw, []byte(c.Placeholder), []byte(c.Secret))
					cambiado = true
				}
			}
			if cambiado {
				nuevo := base64.StdEncoding.EncodeToString(raw)
				return "Basic " + nuevo, [][2]string{{nuevo, enc}}
			}
		}
	}
	for _, c := range cs {
		v = strings.ReplaceAll(v, c.Placeholder, c.Secret)
	}
	return v, nil
}

// variantes son las formas en que un proveedor suele devolver una cadena en
// eco además de tal cual: escapada como JSON (\/ de PHP, < de Go),
// percent-encoded y como entidades HTML. Solo las que cambian algo.
func variantes(s string) []string {
	out := []string{s}
	add := func(v string) {
		for _, o := range out {
			if o == v {
				return
			}
		}
		out = append(out, v)
	}
	if j, err := json.Marshal(s); err == nil && len(j) >= 2 {
		add(string(j[1 : len(j)-1]))
	}
	add(strings.ReplaceAll(s, "/", `\/`))
	add(url.QueryEscape(s))
	add(html.EscapeString(s))
	return out
}

// redactor sustituye cada clave (y sus variantes) por su marcador en un flujo,
// aunque llegue partida entre dos escrituras. Retiene del final de cada trozo
// solo lo que PUEDE ser el comienzo de una clave partida, no un tamaño fijo:
// así un flujo de eventos (SSE) sale entero en cada escritura en vez de con la
// cola de un evento esperando al siguiente. Close/ReadFrom vacían lo retenido.
type redactor struct {
	w     io.Writer
	pares []par
	tail  []byte
}

type par struct{ old, new []byte }

func nuevoRedactor(w io.Writer, cs []Credential) *redactor {
	r := &redactor{w: w}
	for _, c := range cs {
		for _, v := range variantes(c.Secret) {
			r.par(v, c.Placeholder)
		}
	}
	return r
}

// par añade una sustitución old→new. Una vacía o idéntica no aporta nada.
func (r *redactor) par(old, new string) {
	if old == "" || old == new {
		return
	}
	for _, p := range r.pares {
		if string(p.old) == old {
			return
		}
	}
	r.pares = append(r.pares, par{[]byte(old), []byte(new)})
}

// texto redacta una cadena suelta (cabeceras, mensajes de error).
func (r *redactor) texto(s string) string {
	b := []byte(s)
	for _, p := range r.pares {
		b = bytes.ReplaceAll(b, p.old, p.new)
	}
	return string(b)
}

// retener dice cuántos bytes del final de buf hay que guardar porque coinciden
// con el comienzo de alguna clave: lo que sigue podría completarla.
func (r *redactor) retener(buf []byte) int {
	keep := 0
	for _, p := range r.pares {
		lim := min(len(p.old)-1, len(buf))
		for k := lim; k > keep; k-- {
			if buf[len(buf)-k] == p.old[0] && bytes.HasPrefix(p.old, buf[len(buf)-k:]) {
				keep = k
				break
			}
		}
	}
	return keep
}

func (r *redactor) Write(p []byte) (int, error) {
	buf := append(r.tail, p...)
	for _, pr := range r.pares {
		buf = bytes.ReplaceAll(buf, pr.old, pr.new)
	}
	keep := r.retener(buf)
	cut := len(buf) - keep
	if cut > 0 {
		if _, err := r.w.Write(buf[:cut]); err != nil {
			return 0, err
		}
		if f, ok := r.w.(http.Flusher); ok {
			f.Flush()
		}
	}
	r.tail = append([]byte(nil), buf[cut:]...)
	return len(p), nil
}

func (r *redactor) ReadFrom(src io.Reader) (int64, error) {
	var n int64
	b := make([]byte, 32<<10)
	for {
		k, err := src.Read(b)
		if k > 0 {
			if _, werr := r.Write(b[:k]); werr != nil {
				return n, werr
			}
			n += int64(k)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
	}
	if len(r.tail) > 0 {
		_, err := r.w.Write(r.tail)
		r.tail = nil
		return n, err
	}
	return n, nil
}

// credPortStr es credPort en texto, para las reglas.
var credPortStr = strconv.Itoa(credPort)
