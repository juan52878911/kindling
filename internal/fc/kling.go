package fc

// Rutas propias de kling-vz, el ayudante de macOS (docs/backend-vz.md §3).
//
// Firecracker no las conoce y el núcleo solo las llama en macOS: allí no hay
// namespace, veth ni /proc que hagan ese trabajo desde el host, así que se le
// pide al propio VMM. Van en este paquete y no en otro porque viajan por el
// mismo socket y con el mismo formato de error que el resto del API.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
)

// maxRespuestaKling acota lo que se lee de una respuesta del ayudante. Todas
// son objetos pequeños; un ayudante roto que escupiera megas no debe poder
// hacer crecer la memoria del daemon.
const maxRespuestaKling = 1 << 20

// KlingInfo es la respuesta de GET /kling/info.
type KlingInfo struct {
	Backend string `json:"backend"`
	Version string `json:"version"`
	// CredentialKinds son los tipos de credencial que su proxy entiende
	// ("http", "postgres", "mysql", "sigv4"). Un kling-vz anterior no lo manda: solo
	// HTTP.
	CredentialKinds []string `json:"credential_kinds,omitempty"`
}

// KlingNetwork es la política de salida de una máquina (PUT /kling/network).
type KlingNetwork struct {
	Egress       string   `json:"egress"`
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// KlingCredential es una credencial del proxy de credenciales de kling-vz
// (PUT /kling/credentials). Secret es la clave REAL: viaja por el socket de la
// API (0600, del usuario) y se queda en la memoria del ayudante, nunca en la
// del invitado (SECURITY.md §7).
type KlingCredential struct {
	Env         string   `json:"env"`
	Domain      string   `json:"domain"`
	Placeholder string   `json:"placeholder"`
	Secret      string   `json:"secret"`
	Allow       []string `json:"allow,omitempty"`
	// Headers, Query y Body: dónde se cambia el marcador (credproxy.Credential).
	// Solo a un kling-vz que anuncia credproxy.CapHTTPPlaces: uno anterior los
	// ignoraría y lo cambiaría en todas partes.
	Headers     []string `json:"headers,omitempty"`
	Query       bool     `json:"query,omitempty"`
	Body        bool     `json:"body,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Port        int      `json:"port,omitempty"`
	User        string   `json:"user,omitempty"`
	Database    string   `json:"database,omitempty"`
	AnyDatabase bool     `json:"any_database,omitempty"`
	CAPEM       string   `json:"ca_pem,omitempty"`
	// Upstream, UpstreamTLS y TLSServerName: ver credproxy.Credential. Un
	// kling-vz que no anuncia credproxy.CapPostgresUpstream los ignoraría.
	Upstream      string `json:"upstream,omitempty"`
	UpstreamTLS   string `json:"upstream_tls,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`
	// UpstreamMachine y UpstreamOwner: otra máquina de kindling (arista
	// credential, kling db attach). No es una dirección: kling-vz pide la
	// conexión al daemon en cada sesión. Solo a un kling-vz que anuncia
	// credproxy.CapGraphLink.
	UpstreamMachine string `json:"upstream_machine,omitempty"`
	UpstreamOwner   string `json:"upstream_owner,omitempty"`
	// AccessKeyID, Region y Service (Kind "sigv4"): ver credproxy.Credential.
	// Solo a un kling-vz que anuncia credproxy.KindSigV4: uno anterior la
	// serviría como HTTP y cambiaría el marcador por la secret key.
	AccessKeyID string `json:"access_key_id,omitempty"`
	Region      string `json:"region,omitempty"`
	Service     string `json:"service,omitempty"`
}

// KlingGraphLink es una arista link saliente de un nodo.
type KlingGraphLink struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// KlingGraph son las aristas salientes de un nodo (PUT /kling/graph): las
// link, que kling-vz atiende en su pasarela, y los nombres <nodo>.graph que
// su DNS contesta. Sin direcciones: cada conexión se pide al daemon.
type KlingGraph struct {
	Links []KlingGraphLink `json:"links"`
	Hosts []string         `json:"hosts"`
}

// doOut es do() leyendo además el cuerpo de la respuesta en out (si no es nil).
func (c *Client) doOut(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, maxRespuestaKling)
	if resp.StatusCode >= 300 {
		var e struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(lr).Decode(&e)
		return fmt.Errorf("%s %s %s: %s: %s", c.nombre(), method, path, resp.Status, e.FaultMessage)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		return fmt.Errorf("%s %s %s: unreadable response: %w", c.nombre(), method, path, err)
	}
	return nil
}

// nombre es cómo se llama al otro lado en los errores.
func (c *Client) nombre() string {
	if c.quien != "" {
		return c.quien
	}
	return "kling-vz"
}

// KlingInfo pregunta al ayudante qué es y qué versión tiene.
func (c *Client) KlingInfo(ctx context.Context) (*KlingInfo, error) {
	var i KlingInfo
	if err := c.doOut(ctx, http.MethodGet, "/kling/info", nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// SetKlingNetwork fija la política de salida. Tiene que llegar antes de
// InstanceStart o de snapshot/load: sin ella el ayudante aplica "none".
func (c *Client) SetKlingNetwork(ctx context.Context, n KlingNetwork) error {
	return c.doOut(ctx, http.MethodPut, "/kling/network", n, nil)
}

// SetKlingCredentials fija el juego COMPLETO de credenciales del proxy de la
// máquina (sustituye el anterior). Exige que la política ya sea allowlist.
func (c *Client) SetKlingCredentials(ctx context.Context, creds []KlingCredential) error {
	if creds == nil {
		creds = []KlingCredential{}
	}
	return c.doOut(ctx, http.MethodPut, "/kling/credentials", map[string]any{"credentials": creds}, nil)
}

// PutKlingCredentials es SetKlingCredentials devolviendo los dominios que el
// proxy desvía (normalizados y sin repetir).
func (c *Client) PutKlingCredentials(ctx context.Context, creds []KlingCredential) ([]string, error) {
	if creds == nil {
		creds = []KlingCredential{}
	}
	var out struct {
		Domains []string `json:"domains"`
	}
	if err := c.doOut(ctx, http.MethodPut, "/kling/credentials", map[string]any{"credentials": creds}, &out); err != nil {
		return nil, err
	}
	return out.Domains, nil
}

// KlingCredproxy es lo que un VMM con el proxy aparte necesita saber de las
// credenciales de su máquina (PUT /kling/credproxy): qué dominios desviar a
// la pasarela, si alguna es de base de datos (para pasarle también el resto
// de puertos) y si todas van a otra máquina (valen fuera de allowlist). Ni
// claves ni marcadores: las claves las tiene kling-credproxy.
type KlingCredproxy struct {
	Domains      []string `json:"domains"`
	DB           bool     `json:"db,omitempty"`
	MachinesOnly bool     `json:"machines_only,omitempty"`
}

// SetKlingCredproxy enciende (o apaga, sin dominios) el reenvío del VMM al
// proxy de credenciales aparte de su máquina.
func (c *Client) SetKlingCredproxy(ctx context.Context, k KlingCredproxy) error {
	if k.Domains == nil {
		k.Domains = []string{}
	}
	return c.doOut(ctx, http.MethodPut, "/kling/credproxy", k, nil)
}

// KlingHostPort es un puente al host (PUT /kling/host-ports): el puerto Host
// del loopback del Mac en el puerto Guest de la pasarela.
type KlingHostPort struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

// SetKlingHostPorts fija los puentes al host de la máquina (sustituye los
// anteriores).
func (c *Client) SetKlingHostPorts(ctx context.Context, ps []KlingHostPort) error {
	if ps == nil {
		ps = []KlingHostPort{}
	}
	return c.doOut(ctx, http.MethodPut, "/kling/host-ports", map[string]any{"ports": ps}, nil)
}

// SetKlingGraph fija las aristas del nodo (sustituye las anteriores).
func (c *Client) SetKlingGraph(ctx context.Context, g KlingGraph) error {
	if g.Links == nil {
		g.Links = []KlingGraphLink{}
	}
	if g.Hosts == nil {
		g.Hosts = []string{}
	}
	return c.doOut(ctx, http.MethodPut, "/kling/graph", g, nil)
}

// KlingForwards pide un puerto de loopback por cada puerto del invitado y
// devuelve la traducción "puerto del invitado" -> "127.0.0.1:NNNN".
func (c *Client) KlingForwards(ctx context.Context, ports []int) (map[string]string, error) {
	var r struct {
		Forwards map[string]string `json:"forwards"`
	}
	if err := c.doOut(ctx, http.MethodPut, "/kling/forwards", map[string][]int{"ports": ports}, &r); err != nil {
		return nil, err
	}
	// Se comprueba que vengan todos: un reenvío que falta es un puerto al que
	// el daemon creería llegar y no llega, y el fallo aparecería lejos de aquí.
	for _, p := range ports {
		if r.Forwards[strconv.Itoa(p)] == "" {
			return nil, fmt.Errorf("kling-vz did not forward guest port %d", p)
		}
	}
	return r.Forwards, nil
}

// KlingCPU fija el techo de CPU de la máquina en kling-vz, en porcentaje de
// un núcleo (como cpu_pct). Es el cgroup de macOS: ver vz/internal/server/cpu.go.
func (c *Client) KlingCPU(ctx context.Context, pct int) error {
	return c.do(ctx, http.MethodPut, "/kling/cpu", map[string]int{"pct": pct})
}

// KlingCPUEstado es lo que da GET /kling/cpu: el techo, la CPU que lleva
// gastada la VM (CPUMS, -1 si kling-vz no lo sabe) y el pid del auxiliar de
// Apple donde corren sus vCPU (0 si no lo da). ThrottledMS es lo que el
// regulador lleva frenada la VM para cumplir el techo (el throttled_usec del
// cpu.stat de Linux), en ms.
type KlingCPUEstado struct {
	Pct         int   `json:"pct"`
	CPUMS       int64 `json:"cpu_ms"`
	HelperPID   int   `json:"helper_pid"`
	ThrottledMS int64 `json:"throttled_ms"`
}

// KlingCPUState pregunta a kling-vz por su CPU (ver KlingCPUEstado). Un
// kling-vz anterior no da cpu_ms: sale -1, que es "no se sabe".
func (c *Client) KlingCPUState(ctx context.Context) (KlingCPUEstado, error) {
	e := KlingCPUEstado{CPUMS: -1}
	err := c.doOut(ctx, http.MethodGet, "/kling/cpu", nil, &e)
	return e, err
}

// KlingFootprintMiB es la memoria que ocupa la máquina en el host: el
// equivalente en macOS del RSS de firecracker.
func (c *Client) KlingFootprintMiB(ctx context.Context) (int64, error) {
	var s struct {
		FootprintMiB int64 `json:"footprint_mib"`
	}
	if err := c.doOut(ctx, http.MethodGet, "/kling/stats", nil, &s); err != nil {
		return 0, err
	}
	return s.FootprintMiB, nil
}

// KlingProbe pregunta a kling-vz si algo escucha en ese puerto DENTRO del
// invitado. El reenvío de loopback acepta siempre (lo abre el ayudante), así
// que conectar a él no dice nada; esto sí.
func (c *Client) KlingProbe(ctx context.Context, port int) (bool, error) {
	var r struct {
		Open bool `json:"open"`
	}
	if err := c.doOut(ctx, http.MethodGet, "/kling/probe?port="+strconv.Itoa(port), nil, &r); err != nil {
		return false, err
	}
	return r.Open, nil
}

// KlingVirtioFS es una carpeta viva que kling-hv sirve por virtio-fs (PUT
// /kling/shares): su tag (el de share.Attach) y si es de solo lectura.
type KlingVirtioFS struct {
	Tag      int  `json:"tag"`
	ReadOnly bool `json:"read_only,omitempty"`
}

// KlingShares pide a kling-hv, antes de arrancar, un dispositivo virtio-fs por
// carpeta viva (docs/backend-hv.md). kling-vz, y un kling-hv anterior, no lo
// tienen: el error lo dice, y las carpetas van por FUSE como siempre.
func (c *Client) KlingShares(ctx context.Context, shares []KlingVirtioFS) error {
	return c.doOut(ctx, http.MethodPut, "/kling/shares", map[string][]KlingVirtioFS{"shares": shares}, nil)
}

// ErrNoVirtioFS es lo que da KlingShareAttach con un VMM que no sirve esa
// carpeta por virtio-fs.
var ErrNoVirtioFS = errors.New("the VMM does not serve this share over virtio-fs")

// ShareSockBuf son los búferes de envío y recepción de la conexión de una
// carpeta por virtio-fs, en los dos extremos (aquí y en kling-hv): dos
// escrituras máximas de kling-share/1.
const ShareSockBuf = 2 << 20

// KlingShareAttach conecta la sesión proto (kling-share/1) de la carpeta tag
// con el servidor FUSE de su dispositivo virtio-fs en kling-hv: POST
// /kling/share/attach con Upgrade, como el attach al agente. Devuelve la
// conexión, que vive lo que viva la sesión (sin plazo: la corta ctx).
func (c *Client) KlingShareAttach(ctx context.Context, tag int, proto string) (io.ReadWriteCloser, error) {
	body, _ := json.Marshal(map[string]int{"tag": tag})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/kling/share/attach", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)
	// Su propia conexión, con búferes grandes: los de un socket unix de macOS
	// son de 8 KiB (net.local.stream.sendspace), y una escritura de 1 MiB
	// (share.MaxIO) por ahí eran 128 idas y vueltas entre los dos procesos:
	// la escritura secuencial se quedaba en ~300 MB/s.
	h := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dialUnix(ctx, c.sock)
			if uc, ok := conn.(*net.UnixConn); ok {
				_ = uc.SetReadBuffer(ShareSockBuf)
				_ = uc.SetWriteBuffer(ShareSockBuf)
			}
			return conn, err
		},
		DisableKeepAlives: true,
	}}
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		var e struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxRespuestaKling)).Decode(&e)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
			return nil, fmt.Errorf("%w: %s", ErrNoVirtioFS, e.FaultMessage)
		}
		return nil, fmt.Errorf("kling-hv POST /kling/share/attach: %s: %s", resp.Status, e.FaultMessage)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("kling-hv: the share connection could not be taken over")
	}
	return rwc, nil
}
