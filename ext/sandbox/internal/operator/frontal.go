package operator

// Cliente del frontal de kindling-sandbox: la otra mitad del operador, la que
// de verdad crea y borra microVMs. Reutiliza los tipos de petición/respuesta
// de internal/frontal para que el JSON que manda y entiende sea exactamente
// el que el frontal define — sin ese paquete, cualquier drift entre los dos
// (un campo renombrado, un default cambiado) se descubriría en producción.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/ext/sandbox/internal/origen"
	"github.com/juan52878911/kindling/pkg/api"
)

// FrontalClient habla con POST/GET/DELETE /v1/sandboxes del frontal, con el
// token de UN tenant (el que el Deployment del operador trae en su Secret).
type FrontalClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewFrontalClient construye el cliente. baseURL y token vienen de
// KLING_SANDBOX_URL y KLING_SANDBOX_TOKEN (por entorno, nunca por flag: son
// secretos y la URL del tenant, no parámetros de arranque del proceso).
func NewFrontalClient(baseURL, token string) *FrontalClient {
	return &FrontalClient{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: origen.SoloMismoOrigen}}
}

// frontalError es un error del frontal con su código HTTP, para que el que
// llama distinga un 404 (no existe) de un 502 (host caído) sin parsear texto.
type frontalError struct {
	Code    int
	Message string
}

func (e *frontalError) Error() string { return fmt.Sprintf("frontal: %s (%d)", e.Message, e.Code) }

// IsNotFound dice si err es un 404 del frontal: sandbox borrado, ya sea por su
// propio TTL de abandono o a mano.
func IsFrontalNotFound(err error) bool {
	fe, ok := err.(*frontalError)
	return ok && fe.Code == http.StatusNotFound
}

func (f *FrontalClient) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var apiErr api.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		msg := string(b)
		if json.Unmarshal(b, &apiErr) == nil && apiErr.Message != "" {
			msg = apiErr.Message
		}
		return &frontalError{Code: resp.StatusCode, Message: msg}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// LabelK8sUID es la etiqueta con el UID del objeto Sandbox de Kubernetes que
// pidió el sandbox. Es la clave de idempotencia del Create: un sandbox creado
// cuya respuesta se perdió (plazo vencido, status sin escribir) se encuentra
// por ella y se adopta en vez de crear otro.
const LabelK8sUID = "k8s-uid"

// Create pide un sandbox nuevo a partir de spec, etiquetado con uid (si no es
// vacío) para poder encontrarlo después con ByUID.
func (f *FrontalClient) Create(ctx context.Context, spec SandboxSpec, uid string) (*frontal.Sandbox, error) {
	pet := frontal.CrearPeticion{
		Template:   spec.Template,
		Image:      spec.Image,
		TTLSeconds: spec.TTLSeconds,
		OnTTL:      spec.OnTTL,
		Egress:     spec.Egress,
		MemMiB:     spec.MemMiB,
	}
	if uid != "" {
		pet.Labels = map[string]string{LabelK8sUID: uid}
	}
	var sb frontal.Sandbox
	if err := f.do(ctx, http.MethodPost, "/v1/sandboxes", pet, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// List devuelve todos los sandboxes del tenant. completa es false si algún
// host no contestó (X-Kindling-Partial): lo que falta puede existir, y quien
// llama decide si le basta.
func (f *FrontalClient) List(ctx context.Context) (sbs []frontal.Sandbox, completa bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/v1/sandboxes", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, false, &frontalError{Code: resp.StatusCode, Message: string(b)}
	}
	if err := json.NewDecoder(resp.Body).Decode(&sbs); err != nil {
		return nil, false, err
	}
	return sbs, resp.Header.Get("X-Kindling-Partial") == "", nil
}

// ByUID devuelve los sandboxes del tenant etiquetados con uid. completa es
// false si la lista del frontal vino parcial: "no está" no vale entonces para
// crear otro.
func (f *FrontalClient) ByUID(ctx context.Context, uid string) ([]frontal.Sandbox, bool, error) {
	todos, completa, err := f.List(ctx)
	if err != nil {
		return nil, false, err
	}
	return conUID(todos, uid), completa, nil
}

// conUID filtra los de uid.
func conUID(sbs []frontal.Sandbox, uid string) []frontal.Sandbox {
	var out []frontal.Sandbox
	for _, sb := range sbs {
		if sb.Labels[LabelK8sUID] == uid {
			out = append(out, sb)
		}
	}
	return out
}

// parteID es como el frontal nombra hosts y máquinas: ni barras, ni puntos al
// principio, ni nada que haya que escapar.
var parteID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// rutaSandbox es /v1/sandboxes/<host>/<máquina> para un id "host/máquina".
// status.id lo puede escribir cualquiera con permiso sobre sandboxes/status,
// así que no se pega a la URL sin comprobar que es un id y nada más (con una
// barra de más o un "..", la petición iría a otra ruta del frontal).
func rutaSandbox(id string) (string, error) {
	host, ref, ok := strings.Cut(id, "/")
	if !ok || !parteID.MatchString(host) || !parteID.MatchString(ref) {
		return "", fmt.Errorf("%q is not a sandbox id (host/machine)", id)
	}
	return "/v1/sandboxes/" + url.PathEscape(host) + "/" + url.PathEscape(ref), nil
}

// Get trae el estado actual de un sandbox por su id compuesto ("host/máquina").
func (f *FrontalClient) Get(ctx context.Context, id string) (*frontal.Sandbox, error) {
	ruta, err := rutaSandbox(id)
	if err != nil {
		return nil, err
	}
	var sb frontal.Sandbox
	if err := f.do(ctx, http.MethodGet, ruta, nil, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// Renew cambia el TTL de un sandbox vivo (spec.ttlSeconds cambió).
func (f *FrontalClient) Renew(ctx context.Context, id string, ttlSeconds int) (*frontal.Sandbox, error) {
	ruta, err := rutaSandbox(id)
	if err != nil {
		return nil, err
	}
	var sb frontal.Sandbox
	if err := f.do(ctx, http.MethodPost, ruta+"/renew", frontal.RenovarPeticion{TTLSeconds: ttlSeconds}, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// Delete borra el sandbox. Un 404 cuenta como éxito: si ya no está, el
// resultado que quería el que llama (que no exista) ya se cumple, y tratarlo
// como fallo dejaría un finalizer colgado para siempre por algo que la
// limpieza por abandono del propio frontal ya resolvió.
func (f *FrontalClient) Delete(ctx context.Context, id string) error {
	ruta, err := rutaSandbox(id)
	if err != nil {
		return err
	}
	err = f.do(ctx, http.MethodDelete, ruta, nil, nil)
	if err != nil && IsFrontalNotFound(err) {
		return nil
	}
	return err
}
