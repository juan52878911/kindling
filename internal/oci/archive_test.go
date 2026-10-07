package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

func layerTar(files ...ocitest.File) []byte { return ocitest.Tar(files) }

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// subir hace lo que el daemon con PUT /oci/blobs/{digest}: cada blob, solo si
// su sha256 es el de su digest, a cache/sha256/.
func subir(t *testing.T, a *oci.Archive, img *oci.ArchiveImage, cache string) {
	t.Helper()
	os.MkdirAll(filepath.Join(cache, "sha256"), 0o755)
	for _, b := range img.Blobs {
		rc, err := a.Open(b)
		if err != nil {
			t.Fatalf("%s: %v", b.What, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != b.Size || shaOf(body) != b.Digest {
			t.Fatalf("%s: %d bytes %s, declared %d %s", b.What, len(body), shaOf(body), b.Size, b.Digest)
		}
		os.WriteFile(filepath.Join(cache, "sha256", strings.TrimPrefix(b.Digest, "sha256:")), body, 0o644)
	}
}

func escribir(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.tar")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func primeraEntrada(t *testing.T, l oci.Layer) string {
	t.Helper()
	rc, err := oci.OpenLayer(l)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	h, err := tar.NewReader(rc).Next()
	if err != nil {
		t.Fatal(err)
	}
	return h.Name
}

var (
	capaBase  = layerTar(ocitest.File{Name: "bin/", Dir: true, Mode: 0o755}, ocitest.File{Name: "bin/sh", Body: "\x7fELF", Mode: 0o755})
	capaPG    = layerTar(ocitest.File{Name: "pg", Body: "postgres"})
	capaRedis = layerTar(ocitest.File{Name: "redis", Body: "redis"})
)

func dosImagenes() []ocitest.Saved {
	return []ocitest.Saved{
		{Tags: []string{"postgres:17-alpine"}, Arch: "amd64", Config: map[string]any{"Cmd": []string{"postgres"}},
			Layers: [][]byte{capaBase, capaPG, capaBase}},
		{Tags: []string{"redis:7", "redis:latest"}, Arch: "amd64", Config: map[string]any{"Cmd": []string{"redis-server"}},
			Layers: [][]byte{capaBase, capaRedis}},
	}
}

// docker save (hasta la 24): capas sin comprimir, identificadas por diff_id,
// y un manifest.json que no es un manifiesto OCI. Lo que se sube es un
// manifiesto OCI escrito aquí, y con él el constructor tira de la caché sin
// red, comprobando cada blob y los diff_ids.
func TestArchiveDockerSave(t *testing.T) {
	a, err := oci.OpenArchive(escribir(t, ocitest.TarFiles(ocitest.DockerSave(dosImagenes()...))))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Format != oci.FormatDockerArchive {
		t.Fatalf("format %s", a.Format)
	}
	if _, err := a.Image("", "amd64"); err == nil || !strings.Contains(err.Error(), "-image") || !strings.Contains(err.Error(), "redis:7") {
		t.Fatalf("two images without -image: %v", err)
	}
	if _, err := a.Image("mysql:8", "amd64"); err == nil || !strings.Contains(err.Error(), `no image "mysql:8"`) {
		t.Fatalf("an image not in the archive: %v", err)
	}
	if _, err := a.Image("postgres:17-alpine", "arm64"); err == nil || !strings.Contains(err.Error(), "not linux/arm64") {
		t.Fatalf("other arch: %v", err)
	}
	// Por el nombre completo también.
	img, err := a.Image("docker.io/library/postgres:17-alpine", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	cfg := ocitest.SavedConfig(dosImagenes()[0])
	if img.Ref != "postgres:17-alpine" || img.ConfigDigest != shaOf(cfg) || img.LayerBytes != int64(2*len(capaBase)+len(capaPG)) {
		t.Fatalf("%+v", img)
	}
	// manifiesto, configuración y dos capas: la base repetida va una vez.
	if len(img.Blobs) != 4 || img.Blobs[0].What != "manifest" || img.Blobs[2].Digest != shaOf(capaBase) {
		t.Fatalf("blobs %+v", img.Blobs)
	}
	// Por el id de la imagen (docker images).
	if byID, err := a.Image(img.ConfigDigest, "amd64"); err != nil || byID.ManifestDigest != img.ManifestDigest {
		t.Fatalf("by id: %v", err)
	}
	// La capa de redis que es la base de postgres es un enlace dentro del
	// archivo: se resuelve en el índice.
	redis, err := a.Image("redis:latest", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	subir(t, a, img, cache)
	subir(t, a, redis, cache)

	c := &oci.Client{Cache: cache, Offline: true}
	got, err := c.Pull(context.Background(), "docker.io/library/postgres", img.ManifestDigest, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Layers) != 3 || got.Config.Config.Cmd[0] != "postgres" || primeraEntrada(t, got.Layers[1]) != "pg" {
		t.Fatalf("%+v", got)
	}
	if got, err := c.Pull(context.Background(), "x/y", redis.ManifestDigest, "amd64"); err != nil || primeraEntrada(t, got.Layers[1]) != "redis" {
		t.Fatalf("redis: %v", err)
	}
}

// El layout OCI: en un tar (docker save desde la 25) o en un directorio, con
// capas sin comprimir, en gzip o en zstd (docker buildx --output
// type=oci,compression=zstd), y se construye sin red.
func TestArchiveOCILayout(t *testing.T) {
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(b)
		zw.Close()
		return buf.Bytes()
	}
	imgs := dosImagenes()
	imgs[1].Layers = [][]byte{gz(capaBase), ocitest.Zstd(capaRedis)}
	files, mans := ocitest.OCILayout(imgs...)
	dir := t.TempDir()
	if err := ocitest.WriteDir(dir, files); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{escribir(t, ocitest.TarFiles(files)), dir} {
		a, err := oci.OpenArchive(p)
		if err != nil {
			t.Fatal(err)
		}
		if a.Format != oci.FormatOCILayout {
			t.Fatalf("format %s", a.Format)
		}
		if _, err := a.Image("", "amd64"); err == nil || !strings.Contains(err.Error(), "several") {
			t.Fatalf("several: %v", err)
		}
		img, err := a.Image("redis:7", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if img.ManifestDigest != mans[1] || img.Ref != "docker.io/library/redis:7" || len(img.Blobs) != 4 {
			t.Fatalf("%+v", img)
		}
		cache := t.TempDir()
		subir(t, a, img, cache)
		got, err := (&oci.Client{Cache: cache, Offline: true, Unpack: t.TempDir()}).Pull(context.Background(), "x/y", img.ManifestDigest, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if primeraEntrada(t, got.Layers[1]) != "redis" || !strings.Contains(got.Layers[1].MediaType, "zstd") {
			t.Fatalf("redis layer (zstd): %+v", got.Layers[1])
		}
		a.Close()
	}
	// Sin nombre y una sola imagen: esa.
	one := dosImagenes()[:1]
	one[0].Tags = nil
	files, mans = ocitest.OCILayout(one...)
	// Con una atestación al lado, como las que guarda docker save desde la
	// 25: no cuenta como otra imagen.
	var idx map[string]any
	json.Unmarshal(files["index.json"], &idx)
	idx["manifests"] = append(idx["manifests"].([]any), map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest": "sha256:" + strings.Repeat("0", 64), "size": 840,
		"annotations": map[string]string{"io.containerd.manifest.subject": mans[0]}})
	files["index.json"], _ = json.Marshal(idx)
	a, err := oci.OpenArchive(escribir(t, ocitest.TarFiles(files)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if img, err := a.Image("", "amd64"); err != nil || img.ManifestDigest != mans[0] || img.Ref != "" {
		t.Fatalf("the only one: %+v %v", img, err)
	}
}

// Un layout en un directorio no sigue enlaces, ni dentro de él.
func TestArchiveDirNoLinks(t *testing.T) {
	files, _ := ocitest.OCILayout(dosImagenes()[:1]...)
	dir := t.TempDir()
	ocitest.WriteDir(dir, files)
	p := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(shaOf(capaPG), "sha256:"))
	os.Rename(p, filepath.Join(dir, "pg"))
	os.Symlink("../../pg", p)
	a, err := oci.OpenArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Image("", "amd64"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlinked layer: %v", err)
	}
}

func tarCon(t *testing.T, hs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		h.Mode = 0o644
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	tw.Close()
	return buf.Bytes()
}

// Lo que no se acepta de un archivo, antes de subir nada.
func TestArchiveMalformed(t *testing.T) {
	gzipped := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(b)
		zw.Close()
		return buf.Bytes()
	}
	save := func(mod func(map[string][]byte)) []byte {
		f := ocitest.DockerSave(dosImagenes()[:1]...)
		mod(f)
		return ocitest.TarFiles(f)
	}
	layout := func(mod func(map[string][]byte)) []byte {
		f, _ := ocitest.OCILayout(dosImagenes()[:1]...)
		mod(f)
		return ocitest.TarFiles(f)
	}
	var manifest []map[string]any
	json.Unmarshal(ocitest.DockerSave(dosImagenes()[:1]...)["manifest.json"], &manifest)
	cfgName := manifest[0]["Config"].(string)
	layer0 := manifest[0]["Layers"].([]any)[0].(string)
	for _, c := range []struct {
		name string
		tar  []byte
		open string // error de OpenArchive
		img  string // o de Image
	}{
		{"dotdot", tarCon(t, &tar.Header{Name: "manifest.json"}, &tar.Header{Name: "a/../../etc/passwd"}), "names with ..", ""},
		{"absolute", tarCon(t, &tar.Header{Name: "/etc/passwd"}), "absolute", ""},
		{"symlink out", tarCon(t, &tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "../../etc/shadow"}), "out of the archive", ""},
		{"symlink absolute", tarCon(t, &tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"}), "absolute path", ""},
		{"hardlink out", tarCon(t, &tar.Header{Name: "x", Typeflag: tar.TypeLink, Linkname: "../x"}), "hard link", ""},
		{"duplicate", tarCon(t, &tar.Header{Name: "manifest.json"}, &tar.Header{Name: "./manifest.json"}), "twice", ""},
		{"gzip archive", gzipped(save(func(map[string][]byte) {})), "gzip-compressed", ""},
		{"not an archive", tarCon(t, &tar.Header{Name: "hello"}), "not a docker save", ""},
		{"bad manifest.json", save(func(f map[string][]byte) { f["manifest.json"] = []byte("{") }), "manifest.json", ""},
		{"no images", save(func(f map[string][]byte) { f["manifest.json"] = []byte("[]") }), "0 images", ""},
		{"gzip layer", save(func(f map[string][]byte) { f[layer0] = gzipped(f[layer0]) }), "", "compressed (gzip)"},
		{"zstd layer", save(func(f map[string][]byte) { f[layer0] = ocitest.Zstd(f[layer0]) }), "", "compressed (zstd)"},
		{"missing layer", save(func(f map[string][]byte) { delete(f, layer0) }), "", "not in the archive"},
		{"config out", save(func(f map[string][]byte) {
			f["manifest.json"] = bytes.Replace(f["manifest.json"], []byte(cfgName), []byte("../"+cfgName), 1)
		}), "", "names with .."},
		{"diff_ids", save(func(f map[string][]byte) {
			var m []map[string]any
			json.Unmarshal(f["manifest.json"], &m)
			m[0]["Layers"] = m[0]["Layers"].([]any)[:2]
			f["manifest.json"], _ = json.Marshal(m)
		}), "", "2 layers and the config 3 diff_ids"},
		{"layout version", layout(func(f map[string][]byte) { f["oci-layout"] = []byte(`{"imageLayoutVersion":"9"}`) }), "imageLayoutVersion", ""},
		{"layout missing layer", layout(func(f map[string][]byte) {
			delete(f, "blobs/sha256/"+strings.TrimPrefix(shaOf(capaPG), "sha256:"))
		}), "", "is not in the archive"},
		{"layout bad manifest", layout(func(f map[string][]byte) {
			var idx struct{ Manifests []map[string]any }
			json.Unmarshal(f["index.json"], &idx)
			k := "blobs/sha256/" + strings.TrimPrefix(idx.Manifests[0]["digest"].(string), "sha256:")
			f[k] = append(f[k], ' ')
		}), "", "sha256 mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := oci.OpenArchive(escribir(t, c.tar))
			if c.open != "" {
				if err == nil || !strings.Contains(err.Error(), c.open) {
					t.Fatalf("OpenArchive: %v, want %q", err, c.open)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if _, err := a.Image("", "amd64"); err == nil || !strings.Contains(err.Error(), c.img) {
				t.Fatalf("Image: %v, want %q", err, c.img)
			}
		})
	}
}

// Sin red: lo que no está en la caché es un error, sin preguntar a nadie (y
// sin los reintentos de una descarga).
func TestPullOffline(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	man, _ := r.Image("amd64", nil, ocitest.TarGz([]ocitest.File{{Name: "a", Body: "a"}}))
	c := &oci.Client{Cache: t.TempDir(), Offline: true}
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64"); err == nil || !strings.Contains(err.Error(), "not in the daemon's cache") {
		t.Fatalf("offline: %v", err)
	}
	if r.Hits != 0 {
		t.Fatalf("an offline pull hit the registry %d times", r.Hits)
	}
}

// Una capa sin comprimir es su diff_id: un manifiesto que dice otra cosa que
// la configuración no pasa.
func TestPullChecksDiffIDs(t *testing.T) {
	files, mans := ocitest.OCILayout(dosImagenes()[:1]...)
	cache := t.TempDir()
	os.MkdirAll(filepath.Join(cache, "sha256"), 0o755)
	var cfgKey string
	for k, b := range files {
		if strings.HasPrefix(k, "blobs/") {
			os.WriteFile(filepath.Join(cache, strings.TrimPrefix(k, "blobs/")), b, 0o644)
			if bytes.Contains(b, []byte(`"diff_ids"`)) {
				cfgKey = k
			}
		}
	}
	c := &oci.Client{Cache: cache, Offline: true}
	if _, err := c.Pull(context.Background(), "x/y", mans[0], "amd64"); err != nil {
		t.Fatal(err)
	}
	// Otra configuración, con un diff_id cambiado, y el manifiesto que la
	// apunta: todo cuadra por sha256 salvo el diff_id.
	cfg := bytes.Replace(files[cfgKey], []byte(strings.TrimPrefix(shaOf(capaPG), "sha256:")), []byte(strings.Repeat("0", 64)), 1)
	var m map[string]any
	json.Unmarshal(files["blobs/sha256/"+strings.TrimPrefix(mans[0], "sha256:")], &m)
	m["config"].(map[string]any)["digest"] = shaOf(cfg)
	m["config"].(map[string]any)["size"] = len(cfg)
	mb, _ := json.Marshal(m)
	for _, b := range [][]byte{cfg, mb} {
		os.WriteFile(filepath.Join(cache, "sha256", strings.TrimPrefix(shaOf(b), "sha256:")), b, 0o644)
	}
	if _, err := c.Pull(context.Background(), "x/y", shaOf(mb), "amd64"); err == nil || !strings.Contains(err.Error(), "diff_id") {
		t.Fatalf("a layer that is not its diff_id: %v", err)
	}
}
