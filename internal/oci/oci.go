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
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// schema1 dice si un manifiesto es del formato 1 de Docker (obsoleto desde
// 2017: application/vnd.docker.distribution.manifest.v1+json o
// v1+prettyjws). No se piden, pero un registro viejo puede mandarlos igual, y
// sin "layers" acabarían en un "has no layers" que no dice la causa.
func schema1(body []byte, mediaType string) bool {
	if strings.HasPrefix(mediaType, "application/vnd.docker.distribution.manifest.v1+") {
		return true
	}
	var v struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	return json.Unmarshal(body, &v) == nil && v.SchemaVersion == 1
}

// errSchema1 es el error de un manifiesto schema1.
func errSchema1(what string) error {
	return fmt.Errorf("%s is a Docker schema 1 manifest, which is not supported (deprecated since 2017); "+
		"push the image again with a current docker or use a newer tag", what)
}

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
	// Tar, si no está vacío, es la capa ya descomprimida (Client.Unpack):
	// OpenLayer la lee de ahí en vez de descomprimir otra vez.
	Tar string
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
	// Unpack, si no está vacío, es un directorio donde Pull deja además cada
	// capa gzip descomprimida (Layer.Tar), una vez y en paralelo, para que
	// quien la recorre dos veces (el árbol y luego los datos del ext4) no
	// la descomprima dos. Es de quien llama: lo borra él. Con MaxBytes, lo
	// descomprimido no puede pasar de maxUnpackRatio veces el tope (una
	// bomba gzip llenaría el disco).
	Unpack string
	// SiempreRehash: los blobs de la caché se rehashean siempre antes de
	// usarlos. Para el constructor que corre sin privilegios (ver
	// internal/daemon/builders_sinroot.go): la caché es suya, y un constructor
	// comprometido por una imagen podría cambiar un blob —o renombrar uno de
	// root heredado al nombre de otro del mismo tamaño— para envenenar los
	// imports siguientes de otras imágenes. Cuesta rehashear lo cacheado (1-3
	// s en una imagen de GiB); sin él, no se le cree a quien pudo escribirla.
	// No vale para Verificada.
	SiempreRehash bool
	// Verificada, si no está vacío, es una caché de SOLO LECTURA (el
	// directorio que tiene sha256/ dentro) que llena otro, el daemon como
	// root, después de comprobar cada blob por sha256 (ver
	// internal/daemon/builders_cache.go). Se mira antes que Cache, y un blob
	// de ahí se usa sin rehashear aunque SiempreRehash: solo si el fichero,
	// sha256/, el directorio y su padre son de root sin escritura para grupo
	// ni otros (quien construye no puede cambiarlo ni renombrarlo) y el
	// fichero es regular con el tamaño del manifiesto. Si no, cuenta como si
	// no estuviera. En ella no se escribe nunca.
	Verificada string

	mu     sync.Mutex // tokens, hc, Log y usados: las capas se bajan en paralelo
	usados map[string]bool
	authMu sync.Mutex // un solo token pedido a la vez
	tokens map[string]string
	hc     *http.Client
}

// parallel es cuántas capas se bajan a la vez (docker pull baja 3).
const parallel = 4

// maxUnpackRatio es cuánto puede crecer una imagen al descomprimirla,
// respecto a MaxBytes: lo mismo que deja el constructor oci al aplanado.
const maxUnpackRatio = 8

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hc == nil {
		// Uno para todo el Pull: las capas reutilizan las conexiones (y el
		// TLS) del manifiesto y del token.
		c.hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   parallel,
		}, CheckRedirect: checkRedirect}
	}
	return c.hc
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
		c.mu.Lock()
		fmt.Fprintf(c.Log, format+"\n", a...)
		c.mu.Unlock()
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
	for _, p := range []string{c.rutaVerificada(digest), c.BlobPath(digest)} {
		if b, err := leerMax(p, maxManifest); err == nil && sha(b) == digest {
			body = b
			break
		}
	}
	if body == nil {
		var err error
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
	c.usar(digest)
	if schema1(body, mt) {
		return nil, errSchema1(ref + "@" + digest)
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
	}
	img.Layers, err = c.layers(ctx, registry, repo, m.Layers)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// layers baja las capas en paralelo (parallel a la vez) y, con Unpack, las
// descomprime según van llegando (tantas a la vez como núcleos). Cada una se
// descomprime solo después de verificar su sha256. El primer error para las
// demás.
func (c *Client) layers(ctx context.Context, registry, repo string, ds []Descriptor) ([]Layer, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.Unpack != "" {
		if err := os.MkdirAll(c.Unpack, 0o700); err != nil {
			return nil, err
		}
	}
	var budget *atomic.Int64
	if c.MaxBytes > 0 {
		budget = new(atomic.Int64)
		budget.Store(c.MaxBytes * maxUnpackRatio)
	}
	// Una descarga por digest: una imagen puede repetir una capa (las vacías)
	// y dos descargas a la vez al mismo .part se pisarían.
	type fetch struct {
		once sync.Once
		path string
		err  error
	}
	fetches := map[string]*fetch{}
	for _, d := range ds {
		fetches[d.Digest] = &fetch{}
	}
	out := make([]Layer, len(ds))
	dl, cpu := make(chan struct{}, parallel), make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	fail := func(err error) {
		once.Do(func() { first = err; cancel() })
	}
	for i, d := range ds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := fetches[d.Digest]
			f.once.Do(func() {
				select {
				case dl <- struct{}{}:
					f.path, f.err = c.blob(ctx, registry, repo, d)
					<-dl
				case <-ctx.Done():
					f.err = ctx.Err()
				}
			})
			if f.err != nil {
				fail(fmt.Errorf("layer %s: %w", d.Digest, f.err))
				return
			}
			l := Layer{Descriptor: d, Path: f.path}
			if c.Unpack != "" && strings.Contains(d.MediaType, "gzip") {
				select {
				case cpu <- struct{}{}:
				case <-ctx.Done():
					return
				}
				tar := filepath.Join(c.Unpack, fmt.Sprintf("layer-%d.tar", i))
				err := unpack(ctx, l, tar, budget, c.MaxBytes*maxUnpackRatio)
				<-cpu
				if err != nil {
					fail(fmt.Errorf("layer %s: %w", d.Digest, err))
					return
				}
				l.Tar = tar
			}
			out[i] = l
		}()
	}
	wg.Wait()
	if first == nil {
		first = ctx.Err()
	}
	if first != nil {
		return nil, first
	}
	return out, nil
}

// unpack deja la capa l (ya verificada) descomprimida en dst. El CRC32 del
// gzip, que se comprueba al llegar al final, es una segunda defensa contra
// una capa dañada en la caché después de verificarla.
func unpack(ctx context.Context, l Layer, dst string, budget *atomic.Int64, max int64) error {
	rc, err := OpenLayer(l)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, &unpackReader{ctx: ctx, r: rc, left: budget, max: max})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("unpacking: %w", err)
	}
	return nil
}

// unpackReader para si se cancela ctx (otra capa falló) y, con left, descuenta
// lo leído de un presupuesto compartido por todas las capas de la imagen y
// falla al agotarlo.
type unpackReader struct {
	ctx  context.Context
	r    io.Reader
	left *atomic.Int64
	max  int64
}

func (u *unpackReader) Read(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := u.r.Read(p)
	if u.left != nil && u.left.Add(-int64(n)) < 0 {
		return n, fmt.Errorf("the image unpacks to more than %d MiB", u.max>>20)
	}
	return n, err
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

// rutaVerificada es dónde estaría el blob en la caché verificada ("" sin ella).
func (c *Client) rutaVerificada(digest string) string {
	if c.Verificada == "" {
		return ""
	}
	return filepath.Join(c.Verificada, "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// Usados son los digests de los blobs que se han leído o dejado en las
// cachés (manifiestos, configuración y capas), ordenados: lo que el daemon
// verifica y pasa a la caché verificada al acabar bien una construcción.
func (c *Client) Usados() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.usados))
	for d := range c.usados {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func (c *Client) usar(digest string) {
	c.mu.Lock()
	if c.usados == nil {
		c.usados = map[string]bool{}
	}
	c.usados[digest] = true
	c.mu.Unlock()
}

// blob deja el blob en la caché, verificado, y devuelve su ruta.
func (c *Client) blob(ctx context.Context, registry, repo string, d Descriptor) (string, error) {
	if !reDigest.MatchString(d.Digest) {
		return "", fmt.Errorf("invalid digest %q", d.Digest)
	}
	p, err := c.blobSinApuntar(ctx, registry, repo, d)
	if err == nil {
		c.usar(d.Digest)
	}
	return p, err
}

func (c *Client) blobSinApuntar(ctx context.Context, registry, repo string, d Descriptor) (string, error) {
	if p := c.rutaVerificada(d.Digest); p != "" && verificado(c.Verificada, p, d.Size) {
		return p, nil
	}
	dst := c.BlobPath(d.Digest)
	if !c.SiempreRehash && cached(dst, d.Size) {
		return dst, nil
	}
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
			select {
			case <-time.After(time.Duration(try) * 2 * time.Second):
			case <-ctx.Done():
				return "", last
			}
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

// cached dice si el blob de la caché se puede usar sin volver a hashearlo.
//
// Un blob solo llega a su ruta definitiva con un rename después de comprobar
// su sha256 y hacer fsync (download): estar ahí es estar verificado, y
// rehashear cientos de MiB en cada import no protege de nada que no pueda
// hacer ya quien escriba en la caché. La caché es del daemon (root, dentro de
// KLING_ROOT): quien pueda escribir en ella puede cambiar también las
// imágenes ya construidas, el agente o el propio binario. Por si acaso, solo
// se confía sin hashear en un fichero regular (no un enlace), sin escritura
// para grupo ni otros (como los deja download) y con el tamaño que declara el
// manifiesto (un fichero cortado no pasa); si no, se rehashea entero como
// antes. Sin tamaño declarado, también. Con Client.SiempreRehash, nunca.
func cached(p string, size int64) bool {
	if size <= 0 {
		return false
	}
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular() && st.Size() == size && st.Mode().Perm()&0o022 == 0
}

// dueñoVerificada es el dueño que tiene que tener la caché verificada: root
// (variable para los tests, que no corren como root).
var dueñoVerificada uint32 = 0

// verificado dice si el blob p de la caché verificada dir se puede usar sin
// rehashear (Client.Verificada): el fichero, regular, con el tamaño del
// manifiesto; y él, dir/sha256, dir y el padre de dir, de dueñoVerificada y
// sin escritura para grupo ni otros, sin seguir enlaces. Así quien lo usa no
// puede haberlo escrito, ni cambiado, ni puesto otro con su nombre.
func verificado(dir, p string, size int64) bool {
	if size <= 0 {
		return false
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != size || !deDueño(st) {
		return false
	}
	for _, d := range []string{filepath.Join(dir, "sha256"), dir, filepath.Dir(dir)} {
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || !deDueño(st) {
			return false
		}
	}
	return true
}

func deDueño(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == dueñoVerificada && fi.Mode().Perm()&0o022 == 0
}

// leerMax lee un fichero de hasta max bytes, sin seguir un enlace al final.
func leerMax(p string, max int64) ([]byte, error) {
	if p == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return readMax(f, max)
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
		used := c.tokenFor(registry + "/" + repo)
		if used != "" {
			req.Header.Set("Authorization", "Bearer "+used)
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
			if err := c.refreshToken(ctx, auth, registry, repo, used); err != nil {
				return nil, fmt.Errorf("registry token: %w", err)
			}
			continue
		}
		return nil, fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil, errors.New("unauthorized")
}

func (c *Client) tokenFor(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[key]
}

// refreshToken pide un token nuevo, salvo que otra descarga en paralelo ya
// haya cambiado el que se usó (used) mientras se esperaba.
func (c *Client) refreshToken(ctx context.Context, challenge, registry, repo, used string) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	key := registry + "/" + repo
	if c.tokenFor(key) != used {
		return nil
	}
	tok, err := c.token(ctx, challenge, repo)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[key] = tok
	c.mu.Unlock()
	return nil
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

// OpenLayer abre una capa ya verificada como tar (descomprimida si es gzip,
// o la que dejó Unpack). Un tar sin comprimir se da como *os.File: archive/tar
// salta con Seek los datos que no se leen.
func OpenLayer(l Layer) (io.ReadCloser, error) {
	if l.Tar != "" {
		return os.Open(l.Tar)
	}
	f, err := os.Open(l.Path)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(l.MediaType, "gzip") {
		return f, nil
	}
	return newGzip(f)
}
