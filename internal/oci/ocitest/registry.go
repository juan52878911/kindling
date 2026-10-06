// Package ocitest es un registro OCI mínimo en memoria para las pruebas: sirve
// índices, manifiestos y blobs por digest y pide un token Bearer como Docker
// Hub, para probar la descarga sin red.
package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Registry es el registro de prueba.
type Registry struct {
	*httptest.Server
	mu    sync.Mutex
	blobs map[string][]byte
	types map[string]string
	tags  map[string]string
	// Hits cuenta las peticiones a /v2/ (para ver qué salió de la caché).
	Hits int
	// Corrupt, si no está vacío, es un digest cuyo contenido se sirve mal.
	Corrupt string
	// LieDigest, si no está vacío, es lo que dice Docker-Content-Digest al
	// pedir un manifiesto por etiqueta.
	LieDigest string
	// BlobDelay retrasa cada respuesta de /blobs/, y MaxInFlight apunta
	// cuántas hubo a la vez como mucho (para ver las descargas en paralelo).
	BlobDelay   time.Duration
	MaxInFlight int
	inFlight    int
}

// New arranca el registro.
func New() *Registry {
	r := &Registry{blobs: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// Host es "127.0.0.1:puerto", lo que va delante del repositorio.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Tag apunta la etiqueta tag (de cualquier repositorio) al digest.
func (r *Registry) Tag(tag, digest string) {
	r.mu.Lock()
	r.tags[tag] = digest
	r.mu.Unlock()
}

// Put guarda un blob y devuelve su digest.
func (r *Registry) Put(b []byte, mediaType string) string {
	d := digest(b)
	r.mu.Lock()
	r.blobs[d] = b
	r.types[d] = mediaType
	r.mu.Unlock()
	return d
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		json.NewEncoder(w).Encode(map[string]string{"token": "t0k3n"})
		return
	}
	r.mu.Lock()
	r.Hits++
	r.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer t0k3n" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:x:pull"`, r.URL))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	i := strings.LastIndex(req.URL.Path, "/")
	d := req.URL.Path[i+1:]
	r.mu.Lock()
	if t, ok := r.tags[d]; ok && strings.Contains(req.URL.Path, "/manifests/") {
		d = t
		h := d
		if r.LieDigest != "" {
			h = r.LieDigest
		}
		w.Header().Set("Docker-Content-Digest", h)
	}
	b, ok := r.blobs[d]
	mt := r.types[d]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	if d == r.Corrupt {
		b = append([]byte{}, b...)
		b[len(b)/2] ^= 1
	}
	if strings.Contains(req.URL.Path, "/blobs/") {
		r.mu.Lock()
		r.inFlight++
		r.MaxInFlight = max(r.MaxInFlight, r.inFlight)
		delay := r.BlobDelay
		r.mu.Unlock()
		time.Sleep(delay)
		defer func() {
			r.mu.Lock()
			r.inFlight--
			r.mu.Unlock()
		}()
	}
	if mt != "" && strings.Contains(req.URL.Path, "/manifests/") {
		w.Header().Set("Content-Type", mt)
	}
	w.Write(b)
}

// File es una entrada de un tar de prueba.
type File struct {
	Name    string
	Body    string
	Mode    int64
	Uid     int
	Link    string // enlace simbólico
	Dir     bool
	Xattrs  map[string]string
	ModTime time.Time
}

// TarGz arma una capa .tar.gz.
func TarGz(files []File) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: f.Mode, Uid: f.Uid, ModTime: f.ModTime, Format: tar.FormatPAX}
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1700000000, 0)
		}
		switch {
		case f.Dir:
			h.Typeflag = tar.TypeDir
		case f.Link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, f.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(f.Body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if len(f.Xattrs) > 0 {
			h.PAXRecords = map[string]string{}
			for k, v := range f.Xattrs {
				h.PAXRecords["SCHILY.xattr."+k] = v
			}
		}
		tw.WriteHeader(h)
		tw.Write([]byte(f.Body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// Image publica una imagen de una plataforma con esas capas y ese
// entrypoint, y un índice que la contiene. Devuelve los digests del
// manifiesto y del índice.
func (r *Registry) Image(arch string, entrypoint []string, layers ...[]byte) (manifest, index string) {
	return r.ImageConfig(arch, map[string]any{"Entrypoint": entrypoint}, layers...)
}

// ImageConfig es Image con la parte "config" de la configuración entera
// (Entrypoint, Cmd, Env, User, ExposedPorts...).
func (r *Registry) ImageConfig(arch string, config map[string]any, layers ...[]byte) (manifest, index string) {
	cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": config})
	cd := r.Put(cfg, "")
	var ls []map[string]any
	for _, l := range layers {
		ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest": r.Put(l, ""), "size": len(l)})
	}
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers": ls})
	manifest = r.Put(m, "application/vnd.oci.image.manifest.v1+json")
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifest,
			"size": len(m), "platform": map[string]string{"os": "linux", "architecture": arch}}}})
	index = r.Put(idx, "application/vnd.oci.image.index.v1+json")
	return manifest, index
}
