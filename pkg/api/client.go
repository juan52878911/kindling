package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/transport"
)

// Client habla con el daemon. El transporte (socket local o SSH) es
// intercambiable y el resto del CLI no necesita saber cuál está en uso.
type Client struct {
	http *http.Client
	// long sirve a las operaciones que tardan MINUTOS en responder: construir
	// una imagen (instala node y pip en un chroot) y hablar con el invitado
	// (que puede estar arrancando en frío, y cuya herramienta puede ser un
	// escaneo de semgrep sobre un repo entero).
	// El cliente normal acota la espera a las cabeceras para que un daemon
	// atascado no cuelgue a nadie; ese límite es correcto para todo lo demás y
	// letal aquí, así que estas llamadas van por su propio cliente en vez de
	// subirle el número al de todos.
	long *http.Client
	d    *transport.Dialer
}

func NewClient(endpoint string) *Client {
	d := transport.New(endpoint)
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx) }
	c := &Client{
		d: d,
		// Sin ResponseHeaderTimeout: quien lo use acota con su contexto.
		long: &http.Client{Transport: &http.Transport{
			DialContext:       dial,
			DisableKeepAlives: true,
		}},
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.Dial(ctx)
			},
			// Cada petición abre su propia conexión: con SSH detrás, reutilizar
			// conexiones complica más de lo que ahorra.
			DisableKeepAlives: true,

			// ResponseHeaderTimeout y NO http.Client.Timeout.
			//
			// Hace falta un límite: sin ninguno, un daemon atascado deja
			// colgado para siempre a quien le habla, y eso se nota sobre todo
			// al apagar el gateway, que espera a sus llamadas en vuelo.
			//
			// Pero Timeout acota la petición ENTERA, incluida la lectura del
			// cuerpo, y `kling events` es un flujo NDJSON que dura lo que dure
			// la sesión: lo mataría a los 60 s. Esto solo acota la espera a las
			// CABECERAS, que en un flujo llegan de inmediato.
			//
			// Quien añada un endpoint que tarde más de un minuto en responder
			// tiene que pasar su propio cliente, no subir este número.
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
	if tok := os.Getenv(AuthzTokenEnv); tok != "" {
		c.long.Transport = conToken{c.long.Transport, tok}
		c.http.Transport = conToken{c.http.Transport, tok}
	}
	return c
}

// AuthzTokenEnv es la variable con el token de inquilino que el cliente manda
// al daemon (Authorization: Bearer). Solo tiene efecto con una política de
// autorización que lo declare (docs/authz.md); sin ella, el daemon lo ignora.
const AuthzTokenEnv = "KLING_AUTHZ_TOKEN"

// conToken añade el token de inquilino a cada petición al daemon.
type conToken struct {
	rt  http.RoundTripper
	tok string
}

func (t conToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.tok)
	return t.rt.RoundTrip(r)
}

func (c *Client) Endpoint() string { return c.d.Describe() }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWith(c.http, ctx, method, path, body, out)
}

func (c *Client) doWith(cl *http.Client, ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kling"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, http.MethodGet, "/info", nil, &i)
}

func (c *Client) List(ctx context.Context) ([]*Machine, error) {
	var l []*Machine
	return l, c.do(ctx, http.MethodGet, "/machines", nil, &l)
}

// ProcStats trae el consumo de memoria del conjunto para `kling top`. Se sirve
// en JSON (GET /procstats) en vez de parsear el texto de /metrics: el mismo dato,
// sin volver a interpretar el formato de exposición.
func (c *Client) ProcStats(ctx context.Context) (*ProcStats, error) {
	var ps ProcStats
	return &ps, c.do(ctx, http.MethodGet, "/procstats", nil, &ps)
}

func (c *Client) Run(ctx context.Context, r RunRequest) (*Machine, error) {
	var m Machine
	if r.WaitReady {
		// La espera a "listo" puede pasar del minuto de tope de cabeceras
		// (un Android en frío con el Mac cargado): la acota ctx.
		return &m, c.doWith(c.long, ctx, http.MethodPost, "/machines", r, &m)
	}
	return &m, c.do(ctx, http.MethodPost, "/machines", r, &m)
}

// BuildImage empaqueta un servidor MCP de stdio como imagen.
//
// Puede tardar minutos: instala node y sus dependencias dentro de un chroot. El
// ResponseHeaderTimeout del cliente NO lo cubre, así que el daemon responde en
// cuanto termina y quien llame debe darle margen en su contexto.
func (c *Client) BuildImage(ctx context.Context, r BuildImageRequest) (*BuildImageResult, error) {
	var res BuildImageResult
	return &res, c.doWith(c.long, ctx, http.MethodPost, "/images", r, &res)
}

// Images lista las imágenes de rootfs construidas en el daemon.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var l []Image
	return l, c.do(ctx, http.MethodGet, "/images", nil, &l)
}

// ImageRecipe devuelve cómo se construyó una imagen.
func (c *Client) ImageRecipe(ctx context.Context, name string) (*ImageRecipe, error) {
	var rec ImageRecipe
	return &rec, c.do(ctx, http.MethodGet, "/images/"+name+"/recipe", nil, &rec)
}

func (c *Client) Volumes(ctx context.Context) ([]*Volume, error) {
	var l []*Volume
	return l, c.do(ctx, http.MethodGet, "/volumes", nil, &l)
}

// CreateVolume formatea un ext4 nuevo. Va por el cliente largo: mkfs sobre un
// fichero disperso de varios GiB puede pasar del minuto en un disco lento.
func (c *Client) CreateVolume(ctx context.Context, r CreateVolumeRequest) (*Volume, error) {
	var v Volume
	return &v, c.doWith(c.long, ctx, http.MethodPost, "/volumes", r, &v)
}

// PopulateVolume instala paquetes dentro de una microVM desechable.
//
// Va por c.long porque una instalación tarda minutos y el cliente normal corta
// mucho antes: con el cliente de siempre, un `npm install` de un árbol grande
// fallaría por tiempo mientras el daemon lo está haciendo bien.
func (c *Client) PopulateVolume(ctx context.Context, r PopulateRequest) (*PopulateResult, error) {
	var res PopulateResult
	return &res, c.doWith(c.long, ctx, http.MethodPost, "/volumes/"+r.Volume+"/populate", r, &res)
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/volumes/"+name, nil, nil)
}

// RemoveVolumeWithSnapshots borra el volumen Y sus snapshots. Sin esto, un
// volumen con snapshots no se deja borrar: son la única copia de su pasado.
func (c *Client) RemoveVolumeWithSnapshots(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/volumes/"+name+"?snapshots=1", nil, nil)
}

// SnapshotVolume copia un volumen sin escritores. Va por c.long: sin reflink ni
// clonefile es una copia completa, y un volumen grande pasa del minuto.
func (c *Client) SnapshotVolume(ctx context.Context, volume string, r SnapshotVolumeRequest) (*VolumeSnapshot, error) {
	var s VolumeSnapshot
	return &s, c.doWith(c.long, ctx, http.MethodPost, "/volumes/"+volume+"/snapshots", r, &s)
}

// VolumeSnapshots lista los snapshots de un volumen, del más antiguo al último.
func (c *Client) VolumeSnapshots(ctx context.Context, volume string) ([]*VolumeSnapshot, error) {
	var l []*VolumeSnapshot
	return l, c.do(ctx, http.MethodGet, "/volumes/"+volume+"/snapshots", nil, &l)
}

// RestoreVolume devuelve el volumen a un snapshot, guardando antes el estado
// actual en "undo". Por c.long, por lo mismo que SnapshotVolume.
func (c *Client) RestoreVolume(ctx context.Context, volume string, r RestoreVolumeRequest) (*RestoreVolumeResult, error) {
	var res RestoreVolumeResult
	return &res, c.doWith(c.long, ctx, http.MethodPost, "/volumes/"+volume+"/restore", r, &res)
}

// RemoveVolumeSnapshot borra un snapshot de un volumen.
func (c *Client) RemoveVolumeSnapshot(ctx context.Context, volume, snapshot string) error {
	return c.do(ctx, http.MethodDelete, "/volumes/"+volume+"/snapshots/"+snapshot, nil, nil)
}

// Get devuelve una máquina por id, prefijo o nombre.
func (c *Client) Get(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodGet, "/machines/"+ref, nil, &m)
}

func (c *Client) Freeze(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/freeze", nil, &m)
}

// Pause pausa una máquina running sin volcarla (nivel "pausada"; capacidad
// "pause"). Thaw la reanuda.
func (c *Client) Pause(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/pause", nil, &m)
}

func (c *Client) Thaw(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/thaw", nil, &m)
}

func (c *Client) Stop(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/stop", nil, &m)
}

// Squeeze aprieta el globo de una instancia running para devolver RAM al host
// sin congelarla. Va por el cliente largo: al otro lado hay una microVM que
// tarda un par de segundos en entregar sus páginas.
// Resize cambia la memoria de una máquina en caliente, dentro de su techo.
func (c *Client) Resize(ctx context.Context, ref string, memMiB int) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/resize", ResizeRequest{MemMiB: memMiB}, &m)
}

func (c *Client) Squeeze(ctx context.Context, ref string) (*SqueezeResult, error) {
	return c.SqueezeWith(ctx, ref, false)
}

// SqueezeWith es Squeeze; force aprieta también una copia que comparte memoria
// con su dorado (Machine.MemShared), que el daemon rechaza sin él.
func (c *Client) SqueezeWith(ctx context.Context, ref string, force bool) (*SqueezeResult, error) {
	var res SqueezeResult
	path := "/machines/" + ref + "/squeeze"
	if force {
		path += "?force=1"
	}
	return &res, c.doWith(c.long, ctx, http.MethodPost, path, nil, &res)
}

// PutMMDS inyecta el store MMDS (un secreto de sesión) en una microVM viva. data
// es el documento JSON del store; el daemon lo pasa opaco a Firecracker. Tras
// esto la máquina queda marcada HasSecrets y ya no se puede congelar.
func (c *Client) PutMMDS(ctx context.Context, ref string, data any) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/mmds", data, &m)
}

// SetCredentials entrega credenciales al proxy de credenciales de una microVM
// viva con egress allowlist. La clave no entra en el invitado: recibe un
// marcador en la variable de entorno pedida. Se fusionan por Env con las que ya
// tuviera (repetir una la rota). La máquina sigue pudiendo congelarse y las
// credenciales sobreviven al reinicio del daemon.
func (c *Client) SetCredentials(ctx context.Context, ref string, req CredentialsRequest) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/credentials", req, &m)
}

// RemoveCredential quita de una máquina la credencial de variable env. Con
// upstreamMachine no vacío, el daemon exige que esa credencial vaya a esa
// máquina (kling db detach). En una máquina viva el proxy corta en el acto las
// sesiones de Postgres que la usaban.
func (c *Client) RemoveCredential(ctx context.Context, ref, env, upstreamMachine string) (*Machine, error) {
	path := "/machines/" + url.PathEscape(ref) + "/credentials/" + url.PathEscape(env)
	if upstreamMachine != "" {
		path += "?upstream_machine=" + url.QueryEscape(upstreamMachine)
	}
	var m Machine
	return &m, c.do(ctx, http.MethodDelete, path, nil, &m)
}

// SetSnapshotCredentials ata credenciales a una plantilla: cada instancia que
// nazca de ella (kling run -from, el gateway MCP) las recibe en su proxy de
// credenciales al arrancar. Exige que la plantilla tenga egress allowlist.
func (c *Client) SetSnapshotCredentials(ctx context.Context, name string, req CredentialsRequest) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPut, "/snapshots/"+name+"/credentials", req, &s)
}

func (c *Client) Remove(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/machines/"+ref, nil, nil)
}

// maxLogsResponse acota lo que Logs() lee de la respuesta del daemon. El
// daemon ya acota firecracker.log por su lado (logMaxBytes, logMaxLines en
// internal/machine/logs.go), pero sin este segundo tope una CLI o SDK contra
// un daemon viejo o comprometido cargaría la respuesta entera en memoria
// (D-04/A-01: el lado cliente del mismo M-07).
const maxLogsResponse = 8 << 20

// Logs trae la consola serie. Se devuelve texto plano, no JSON: es para leer.
func (c *Client) Logs(ctx context.Context, ref string, tail int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://kling/machines/%s/logs?tail=%d", ref, tail), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxLogsResponse))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return "", fmt.Errorf("%s", e.Message)
		}
		return "", fmt.Errorf("%s", resp.Status)
	}
	return string(b), nil
}

// maxCredAuditResponse acota lo que CredAudit lee del daemon, que ya acota por
// su lado (4 MiB de fichero): mismo motivo que maxLogsResponse.
const maxCredAuditResponse = 8 << 20

// CredAudit trae el registro de auditoría del proxy de credenciales de una
// máquina (GET /machines/{ref}/credaudit), filtrado por q, las más antiguas
// primero. Un daemon anterior no conoce la ruta: el error lo dice.
func (c *Client) CredAudit(ctx context.Context, ref string, q CredAuditQuery) ([]CredAuditRecord, error) {
	v := url.Values{}
	v.Set("tail", strconv.Itoa(q.Tail))
	if q.Denied {
		v.Set("denied", "1")
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://kling/machines/"+url.PathEscape(ref)+"/credaudit?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxCredAuditResponse)
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(body)
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return nil, fmt.Errorf("%s", e.Message)
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("the daemon has no credential audit log endpoint: update it")
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var out []CredAuditRecord
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r CredAuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("credential audit log: %w", err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// SetLabels reetiqueta una máquina. Se usa al importar, cuando la decisión sobre
// el modo de ejecución solo puede tomarse DESPUÉS de ver su catálogo.
func (c *Client) SetLabels(ctx context.Context, ref string, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, "/machines/"+ref+"/labels", labels, nil)
}

// Commit congela una máquina como snapshot dorado. replace permite pisar uno
// existente con el mismo nombre (el daemon se niega si tiene instancias vivas).
func (c *Client) Commit(ctx context.Context, ref, name string, replace bool) (*Snapshot, error) {
	return c.CommitWith(ctx, ref, CommitRequest{Name: name, Replace: replace})
}

// CommitWith es Commit con la petición entera (skip_ready, ready_timeout_seconds).
// Sin tope de cabeceras: el daemon espera a que el invitado esté listo según su
// imagen antes de congelarlo, y eso puede ser más de un minuto. Acota ctx.
func (c *Client) CommitWith(ctx context.Context, ref string, req CommitRequest) (*Snapshot, error) {
	var s Snapshot
	return &s, c.doWith(c.long, ctx, http.MethodPost, "/machines/"+ref+"/commit", req, &s)
}

// Ready pregunta si la máquina ref está lista según su imagen (sonda y ganchos
// tras restaurar); con wait > 0 espera hasta ese plazo. Un "no listo" no es un
// error: mira ReadyResult.OK.
func (c *Client) Ready(ctx context.Context, ref string, wait time.Duration) (*ReadyResult, error) {
	var res ReadyResult
	path := "/machines/" + ref + "/ready"
	if wait > 0 {
		path += "?wait=" + url.QueryEscape(wait.String())
	}
	return &res, c.doWith(c.long, ctx, http.MethodGet, path, nil, &res)
}

// RunHooks vuelve a lanzar los ganchos tras restaurar de la máquina ref y, con
// wait > 0, espera a que acaben.
func (c *Client) RunHooks(ctx context.Context, ref string, wait time.Duration) (*ReadyResult, error) {
	var res ReadyResult
	path := "/machines/" + ref + "/hooks"
	if wait > 0 {
		path += "?wait=" + url.QueryEscape(wait.String())
	}
	return &res, c.doWith(c.long, ctx, http.MethodPost, path, nil, &res)
}

func (c *Client) Snapshots(ctx context.Context) ([]*Snapshot, error) {
	var l []*Snapshot
	return l, c.do(ctx, http.MethodGet, "/snapshots", nil, &l)
}

// RemoveImage retira una imagen del disco.
func (c *Client) RemoveImage(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/images/"+name, nil, nil)
}

func (c *Client) RemoveSnapshot(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/snapshots/"+name, nil, nil)
}

// Events consume el stream NDJSON del daemon hasta que se cancele el contexto.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kling/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) == nil {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && err != io.EOF {
		return err
	}
	return nil
}

// Guest habla con el servidor que corre dentro de una microVM, pasando por el
// daemon. Es la única vía que funciona igual en local y por SSH.
func (c *Client) Guest(ctx context.Context, ref string, r GuestRequest) (*GuestResponse, error) {
	var out GuestResponse
	// Cliente largo: al otro lado hay una microVM, no el daemon. Puede estar
	// descongelándose, y la herramienta que se invoca puede tardar lo suyo.
	// Acotar esto por cabeceras es acotar el trabajo del usuario, no la salud
	// del daemon.
	if err := c.doWith(c.long, ctx, "POST", "/machines/"+ref+"/guest", r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
