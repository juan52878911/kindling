// Package oci baja imágenes de un registro OCI (Docker Hub, ghcr.io...) sin
// Docker: el manifiesto por digest, la configuración y las capas, cada pieza
// comprobada con su sha256 antes de usarla, y las capas en una caché por
// hash para no volver a bajarlas.
//
// Solo lectura y solo por digest: una etiqueta se puede mover, un digest no.
// La única traducción etiqueta → digest es Resolve (ref.go), y el digest que
// da sale del manifiesto bajado, no de lo que diga el registro.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// Tipos de manifiesto que se aceptan.
const (
	MediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var reDigest = lazyre.New(`^sha256:[0-9a-f]{64}$`)

// manifestAccept son los tipos de manifiesto que se piden.
var manifestAccept = strings.Join([]string{MediaOCIManifest, MediaDockerManifest, MediaOCIIndex, MediaDockerList}, ", ")

// maxManifest es el tamaño máximo de un manifiesto o un índice.
const maxManifest = 4 << 20

// maxConfig es el tamaño máximo del blob de configuración: se lee entero a
// memoria (los de Docker Hub rondan los 10 KiB).
const maxConfig = 8 << 20

// Descriptor es una pieza referida por digest.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

// Manifest es el manifiesto de una imagen de una plataforma, o un índice.
type Manifest struct {
	MediaType string       `json:"mediaType"`
	Config    Descriptor   `json:"config"`
	Layers    []Descriptor `json:"layers"`
	Manifests []Descriptor `json:"manifests"`
}

// Config es lo que interesa de la configuración de la imagen.
type Config struct {
	Architecture string      `json:"architecture"`
	OS           string      `json:"os"`
	Variant      string      `json:"variant,omitempty"`
	Config       ImageConfig `json:"config"`
}

// ImageConfig es la parte "config" de la configuración de una imagen: lo
// que Docker usa para ejecutarla (docker run).
type ImageConfig struct {
	Entrypoint   []string            `json:"Entrypoint"`
	Cmd          []string            `json:"Cmd"`
	Env          []string            `json:"Env"`
	User         string              `json:"User,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	Healthcheck  *Healthcheck        `json:"Healthcheck,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
}

// Healthcheck es el HEALTHCHECK de un Dockerfile. Test es ["NONE"],
// ["CMD", arg...] o ["CMD-SHELL", "orden"]; los plazos en nanosegundos.
type Healthcheck struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
}

// Image es una imagen bajada y verificada.
type Image struct {
	Ref            string // registro/repo@digest del manifiesto
	ManifestDigest string
	Manifest       Manifest
	Config         Config
	// Layers son las rutas de las capas en la caché, en orden.
	Layers []Layer
}

// Layer es una capa verificada en la caché.
type Layer struct {
	Descriptor
	Path string
}

// Client habla con los registros.
type Client struct {
	// Cache es el directorio de blobs (se crea si falta).
	Cache string
	// Log recibe una línea por descarga (nil = nada).
	Log io.Writer
	// HTTP para las pruebas; nil = uno con plazos.
	HTTP *http.Client
	// MaxBytes es el tope de lo que se baja de una imagen (las capas
	// comprimidas, por lo que declara el manifiesto): 0 = sin tope. Con
	// tope, una capa sin tamaño declarado no se acepta.
	MaxBytes int64

	tokens map[string]string
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}, CheckRedirect: checkRedirect}
}

// checkRedirect sigue las redirecciones de los registros (las capas suelen
// estar en un CDN) pero nunca de https a http: el contenido se verifica por
// sha256, pero el token de la petición no debe viajar en claro.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" && !(req.URL.Scheme == "http" && isLocalHost(req.URL.Hostname()) && isLocalHost(via[0].URL.Hostname())) {
		return fmt.Errorf("refusing redirect to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// isLocalHost dice si host (sin puerto) es esta máquina: solo ahí se habla
// http (un registro de pruebas). "localhost.example.com" no lo es.
func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// registryHost quita el puerto de "registro[:puerto]".
func registryHost(registry string) string {
	if h, _, err := net.SplitHostPort(registry); err == nil {
		return h
	}
	return strings.Trim(registry, "[]")
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, format+"\n", a...)
	}
}

// ParseRef parte "registro/repo" en registro y repositorio; sin registro es
// Docker Hub (y "debian" es "library/debian").
func ParseRef(ref string) (registry, repo string) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry, repo = parts[0], parts[1]
	} else {
		registry, repo = "registry-1.docker.io", ref
	}
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = "registry-1.docker.io"
	}
	if registry == "registry-1.docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return registry, repo
}

// Pull baja la imagen repo@digest para linux/arch. digest puede ser el de un
// manifiesto de una plataforma o el de un índice (se elige arch dentro, y la
// entrada elegida se comprueba con el digest que declara el índice ya
// verificado).
func (c *Client) Pull(ctx context.Context, ref, digest, arch string) (*Image, error) {
	if !reDigest.MatchString(digest) {
		return nil, fmt.Errorf("invalid digest %q (want sha256:<64 hex>)", digest)
	}
	registry, repo := ParseRef(ref)
	// Un manifiesto es contenido direccionado por su hash como cualquier
	// blob: si ya está en la caché y cuadra, no hace falta ni el registro
	// (reconstruir sin red).
	var body []byte
	var mt string
	if b, err := os.ReadFile(c.BlobPath(digest)); err == nil && sha(b) == digest {
		body = b
	} else {
		body, mt, err = c.get(ctx, registry, repo, "manifests/"+digest, manifestAccept, maxManifest)
		if err != nil {
			return nil, err
		}
		if got := sha(body); got != digest {
			return nil, fmt.Errorf("manifest %s: sha256 mismatch (got %s)", digest, got)
		}
		if err := os.MkdirAll(filepath.Dir(c.BlobPath(digest)), 0o755); err == nil {
			tmp := c.BlobPath(digest) + ".part"
			if os.WriteFile(tmp, body, 0o644) == nil {
				_ = os.Rename(tmp, c.BlobPath(digest))
			}
		}
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	if m.MediaType == "" {
		m.MediaType = mt
	}
	if len(m.Manifests) > 0 {
		pick := pickPlatform(m.Manifests, arch)
		if pick == nil {
			return nil, fmt.Errorf("%s@%s has no linux/%s image", ref, digest, arch)
		}
		c.logf("index %s: linux/%s is %s", short(digest), arch, pick.Digest)
		return c.Pull(ctx, ref, pick.Digest, arch)
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("manifest %s has no layers (media type %s)", digest, m.MediaType)
	}
	if c.MaxBytes > 0 {
		var total int64
		for _, l := range m.Layers {
			if l.Size <= 0 {
				return nil, fmt.Errorf("layer %s has no declared size", l.Digest)
			}
			total += l.Size
		}
		if total > c.MaxBytes {
			return nil, fmt.Errorf("image %s is %d MiB compressed, over the %d MiB limit", digest, total>>20, c.MaxBytes>>20)
		}
	}
	img := &Image{Ref: registry + "/" + repo + "@" + digest, ManifestDigest: digest, Manifest: m}
	if m.Config.Size <= 0 || m.Config.Size > maxConfig {
		return nil, fmt.Errorf("config %s: declared size %d, want 1..%d bytes", m.Config.Digest, m.Config.Size, maxConfig)
	}
	cfgPath, err := c.blob(ctx, registry, repo, m.Config)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cb, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cb, &img.Config); err != nil {
		return nil, fmt.Errorf("config %s: %w", m.Config.Digest, err)
	}
	if img.Config.Architecture != arch || (img.Config.OS != "" && img.Config.OS != "linux") {
		return nil, fmt.Errorf("image %s is %s/%s, not linux/%s", digest, img.Config.OS, img.Config.Architecture, arch)
	}
	for _, l := range m.Layers {
		if !strings.Contains(l.MediaType, "tar") || strings.Contains(l.MediaType, "zstd") {
			return nil, fmt.Errorf("layer %s: unsupported media type %q (only tar and tar+gzip)", l.Digest, l.MediaType)
		}
		p, err := c.blob(ctx, registry, repo, l)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		img.Layers = append(img.Layers, Layer{Descriptor: l, Path: p})
	}
	return img, nil
}

// pickPlatform elige la imagen linux/arch de un índice. En arm64 vale la
// variante v8 o ninguna; en amd64, la que no declara variante antes que una
// v2/v3 (que pide instrucciones que puede no haber). Las atestaciones
// (unknown/unknown) no casan nunca.
func pickPlatform(ds []Descriptor, arch string) *Descriptor {
	var pick *Descriptor
	for i, d := range ds {
		p := d.Platform
		if p == nil || p.OS != "linux" || p.Architecture != arch {
			continue
		}
		if p.Variant == "" || (arch == "arm64" && p.Variant == "v8") {
			return &ds[i]
		}
		if pick == nil {
			pick = &ds[i]
		}
	}
	return pick
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func short(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// BlobPath es dónde queda un blob en la caché.
func (c *Client) BlobPath(digest string) string {
	return filepath.Join(c.Cache, "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// blob deja el blob en la caché, verificado, y devuelve su ruta.
func (c *Client) blob(ctx context.Context, registry, repo string, d Descriptor) (string, error) {
	if !reDigest.MatchString(d.Digest) {
		return "", fmt.Errorf("invalid digest %q", d.Digest)
	}
	dst := c.BlobPath(d.Digest)
	if ok, _ := fileHas(dst, d.Digest, d.Size); ok {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	var last error
	for try := 0; try < 3; try++ {
		if try > 0 {
			c.logf("retrying %s: %v", short(d.Digest), last)
			time.Sleep(time.Duration(try) * 2 * time.Second)
		}
		last = c.download(ctx, registry, repo, d, dst)
		if last == nil {
			return dst, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return "", last
}

func fileHas(p, digest string, size int64) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || (size > 0 && st.Size() != size) {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return "sha256:"+hex.EncodeToString(h.Sum(nil)) == digest, nil
}

// stall es cuánto puede estar una descarga sin recibir nada.
const stall = 60 * time.Second

func (c *Client) download(ctx context.Context, registry, repo string, d Descriptor, dst string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := c.do(ctx, registry, repo, "blobs/"+d.Digest, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	defer f.Close()
	t0 := time.Now()
	timer := time.AfterFunc(stall, cancel)
	defer timer.Stop()
	h := sha256.New()
	limit := d.Size
	if limit <= 0 {
		limit = 64 << 30
	}
	n, err := io.Copy(io.MultiWriter(f, h), &stallReader{r: io.LimitReader(resp.Body, limit+1), t: timer})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("download stalled (no data for %s)", stall)
		}
		return err
	}
	if d.Size > 0 && n != d.Size {
		return fmt.Errorf("size %d, manifest says %d", n, d.Size)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return fmt.Errorf("sha256 mismatch: got %s", got)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	c.logf("downloaded %s (%d MiB in %.1f s), sha256 verified", short(d.Digest), n>>20, time.Since(t0).Seconds())
	return nil
}

type stallReader struct {
	r io.Reader
	t *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.t.Reset(stall)
	}
	return n, err
}

func (c *Client) get(ctx context.Context, registry, repo, path, accept string, max int64) ([]byte, string, error) {
	resp, err := c.do(ctx, registry, repo, path, accept)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := readMax(resp.Body, max)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return b, resp.Header.Get("Content-Type"), nil
}

func readMax(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("response too large")
	}
	return b, nil
}

// do hace la petición y, si el registro pide un token (401 con Bearer),
// lo consigue y repite.
func (c *Client) do(ctx context.Context, registry, repo, path, accept string) (*http.Response, error) {
	u := "https://" + registry + "/v2/" + repo + "/" + path
	if isLocalHost(registryHost(registry)) {
		u = "http://" + registry + "/v2/" + repo + "/" + path
	}
	for try := 0; try < 2; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if t := c.tokens[registry+"/"+repo]; t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := c.client().Do(req)
		if err != nil && ctx.Err() == nil {
			// Un corte de red (el TLS de Docker Hub a veces no contesta):
			// otra vez, una sola.
			time.Sleep(2 * time.Second)
			resp, err = c.client().Do(req.Clone(ctx))
		}
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		auth := resp.Header.Get("WWW-Authenticate")
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && try == 0 && strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			tok, err := c.token(ctx, auth, repo)
			if err != nil {
				return nil, fmt.Errorf("registry token: %w", err)
			}
			if c.tokens == nil {
				c.tokens = map[string]string{}
			}
			c.tokens[registry+"/"+repo] = tok
			continue
		}
		return nil, fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil, errors.New("unauthorized")
}

// token pide un token anónimo de lectura al servicio que indica el 401.
func (c *Client) token(ctx context.Context, challenge, repo string) (string, error) {
	params := map[string]string{}
	for _, kv := range splitChallenge(challenge[len("bearer "):]) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm := params["realm"]
	ru, err := url.Parse(realm)
	if err == nil && ru.Scheme == "http" && isLocalHost(ru.Hostname()) {
		err = nil // un registro local de pruebas
	} else if err == nil && ru.Scheme != "https" {
		err = errors.New("not https")
	}
	if err != nil {
		return "", fmt.Errorf("unexpected token realm %q", realm)
	}
	q := ru.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	ru.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil && ctx.Err() == nil {
		time.Sleep(2 * time.Second)
		resp, err = c.client().Do(req.Clone(ctx))
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", ru.Host, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", errors.New("empty token")
	}
	return t.Token, nil
}

// splitChallenge parte `realm="a",service="b",scope="c,d"` respetando comillas.
func splitChallenge(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// OpenLayer abre una capa ya verificada como tar (descomprimida si es gzip).
func OpenLayer(l Layer) (io.ReadCloser, error) {
	f, err := os.Open(l.Path)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(l.MediaType, "gzip") {
		return f, nil
	}
	return newGzip(f)
}
