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
	"time"

	"github.com/juan52878911/kindling/internal/transport"
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
	return c
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

func (c *Client) Run(ctx context.Context, r RunRequest) (*Machine, error) {
	var m Machine
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

// ImageRecipe devuelve cómo se construyó una imagen.
func (c *Client) ImageRecipe(ctx context.Context, name string) (*ImageRecipe, error) {
	var rec ImageRecipe
	return &rec, c.do(ctx, http.MethodGet, "/images/"+name+"/recipe", nil, &rec)
}

// ImageCapabilities devuelve lo que una imagen declara necesitar (navegador,
// internet, módulos nativos), detectado al construirla. El import lo usa para
// configurar el egress automáticamente.
func (c *Client) ImageCapabilities(ctx context.Context, name string) (*Capabilities, error) {
	var caps Capabilities
	return &caps, c.do(ctx, http.MethodGet, "/images/"+name+"/capabilities", nil, &caps)
}

// RefreshBridges pone el puente actual dentro de las imágenes ya construidas.
//
// Por c.long: monta y desmonta cada imagen, y con varias grandes pasa del plazo
// del cliente normal.
func (c *Client) RefreshBridges(ctx context.Context, images []string) ([]BridgeRefresh, error) {
	var res []BridgeRefresh
	body := struct {
		Images []string `json:"images,omitempty"`
	}{images}
	return res, c.doWith(c.long, ctx, http.MethodPost, "/images/refresh-bridge", body, &res)
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

func (c *Client) Freeze(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/freeze", nil, &m)
}

func (c *Client) Thaw(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/thaw", nil, &m)
}

func (c *Client) Stop(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/stop", nil, &m)
}

func (c *Client) Remove(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/machines/"+ref, nil, nil)
}

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
	b, err := io.ReadAll(resp.Body)
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

// SetLabels reetiqueta una máquina. Se usa al importar, cuando la decisión sobre
// el modo de ejecución solo puede tomarse DESPUÉS de ver su catálogo.
func (c *Client) SetLabels(ctx context.Context, ref string, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, "/machines/"+ref+"/labels", labels, nil)
}

func (c *Client) Commit(ctx context.Context, ref, name string) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPost, "/machines/"+ref+"/commit", CommitRequest{Name: name}, &s)
}

func (c *Client) Snapshots(ctx context.Context) ([]*Snapshot, error) {
	var l []*Snapshot
	return l, c.do(ctx, http.MethodGet, "/snapshots", nil, &l)
}

func (c *Client) Links(ctx context.Context) ([]*Link, error) {
	var l []*Link
	return l, c.do(ctx, http.MethodGet, "/links", nil, &l)
}

func (c *Client) SetLink(ctx context.Context, l *Link) (*Link, error) {
	var out Link
	return &out, c.do(ctx, http.MethodPut, "/links", l, &out)
}

func (c *Client) RemoveLink(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/links/"+name, nil, nil)
}

func (c *Client) SetCatalog(ctx context.Context, name string, tools []ToolSpec) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPut, "/snapshots/"+name+"/catalog", CatalogRequest{Tools: tools}, &s)
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
