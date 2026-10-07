package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/pkg/api"
)

// blobsMux hace de daemon para la subida: /info y GET/PUT /oci/blobs, que
// deja en cache/sha256 lo que cuadra con su digest, como el de verdad.
type blobsMux struct {
	mu    sync.Mutex
	cache string
	caps  []string
	puts  []string
}

func (b *blobsMux) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Info{Version: "dev", Arch: "amd64", Capabilities: b.caps})
	})
	path := func(d string) string {
		return filepath.Join(b.cache, "sha256", strings.TrimPrefix(d, "sha256:"))
	}
	m.HandleFunc("GET /oci/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		fi, err := os.Stat(path(r.PathValue("digest")))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no"}`))
			return
		}
		json.NewEncoder(w).Encode(api.OCIBlob{Digest: r.PathValue("digest"), Size: fi.Size()})
	})
	m.HandleFunc("PUT /oci/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		d := r.PathValue("digest")
		body, _ := io.ReadAll(r.Body)
		h := sha256.Sum256(body)
		if "sha256:"+hex.EncodeToString(h[:]) != d || int64(len(body)) != r.ContentLength {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"sha256 mismatch"}`))
			return
		}
		os.MkdirAll(filepath.Join(b.cache, "sha256"), 0o755)
		os.WriteFile(path(d), body, 0o644)
		b.mu.Lock()
		b.puts = append(b.puts, d)
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.OCIBlob{Digest: d, Size: int64(len(body))})
	})
	return m
}

func dockerSaveDe(t *testing.T, imgs ...ocitest.Saved) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "imagen.tar")
	if err := os.WriteFile(p, ocitest.TarFiles(ocitest.DockerSave(imgs...)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// De punta a punta sin red: un docker save en la máquina del CLI, los blobs
// subidos a la caché del "daemon" y el constructor oci que construye desde
// ella. La receta dice "archive" y el digest, nunca la ruta del archivo.
func TestImportArchiveBuild(t *testing.T) {
	for _, aislado := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "builder user"}[aislado], func(t *testing.T) {
			e := newOCITest(t)
			if aislado {
				// El constructor sin privilegios tiene su caché: lo subido
				// lo lee de la de root (Seed), sin copiarlo.
				t.Setenv("KLING_CACHE_DIR", t.TempDir())
			}
			archivo := dockerSaveDe(t, ocitest.Saved{Tags: []string{"postgres:17-alpine"}, Arch: "amd64",
				Config: map[string]any{"Cmd": []string{"postgres"}, "ExposedPorts": map[string]any{"5432/tcp": map[string]any{}}},
				Layers: [][]byte{ocitest.Tar(alpineLike()), ocitest.Tar([]ocitest.File{{Name: "var/.wh.old"}, {Name: "pg", Body: "x"}})}})
			d := &blobsMux{cache: filepath.Join(e.root, "cache", "oci"), caps: []string{api.CapabilityOCIBlobs}}
			c := fakeDaemon(t, d.handler())
			ctx := context.Background()
			ai, err := abrirArchivoImport(ctx, c, archivo, "", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer ai.a.Close()
			if n, _, err := ai.subir(ctx, c, nil); err != nil || n != 4 {
				t.Fatalf("upload: %d %v", n, err)
			}
			// Otra vez: el daemon ya los tiene, no se manda nada.
			if n, _, err := ai.subir(ctx, c, nil); err != nil || n != 0 {
				t.Fatalf("second upload: %d %v", n, err)
			}
			spec := OCISpec{Ref: ai.img.Ref, Digest: ai.img.ManifestDigest, Source: ociSourceArchive, Arch: ai.arch}
			hints, log, err := e.build(nombreArchivo(ai.img), spec)
			if err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			if e.reg.Hits != 0 {
				t.Fatalf("an archive build hit the registry %d times", e.reg.Hits)
			}
			var built map[string]any
			json.Unmarshal(hints.Built, &built)
			if built["source"] != "archive" || built["config"] != ai.img.ConfigDigest || built["digest"] != ai.img.ManifestDigest ||
				!strings.Contains(built["ref"].(string), "postgres:17-alpine") || built["ready"] != "tcp 5432" {
				t.Fatalf("built %s", hints.Built)
			}
			req, _ := os.ReadFile(filepath.Join(e.work, "request.json"))
			for _, b := range [][]byte{req, hints.Built} {
				if strings.Contains(string(b), filepath.Dir(archivo)) || strings.Contains(string(b), "imagen.tar") {
					t.Fatalf("the host path leaked: %s", b)
				}
			}
			if _, err := os.Stat(filepath.Join(e.root, "images", "postgres-17-alpine.ext4")); err != nil {
				t.Fatal(err)
			}
			if aislado {
				if es, _ := os.ReadDir(filepath.Join(os.Getenv("KLING_CACHE_DIR"), "oci", "sha256")); len(es) != 0 {
					t.Fatalf("the uploaded blobs were copied to the builder's cache: %d", len(es))
				}
			}
		})
	}
}

// Sin los blobs en la caché (se borró, o la receta es de otro daemon), el
// constructor no va a ningún registro: lo dice.
func TestImportArchiveBuildMissingBlob(t *testing.T) {
	e := newOCITest(t)
	archivo := dockerSaveDe(t, ocitest.Saved{Tags: []string{"x:1"}, Arch: "amd64", Layers: [][]byte{ocitest.Tar(alpineLike())}})
	a, err := oci.OpenArchive(archivo)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	img, _ := a.Image("", "amd64")
	_, log, err := e.build("x", OCISpec{Ref: img.Ref, Digest: img.ManifestDigest, Source: ociSourceArchive, Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "import the archive again") {
		t.Fatalf("%v\n%s", err, log)
	}
}

func TestImportArchiveChecks(t *testing.T) {
	ctx := context.Background()
	archivo := dockerSaveDe(t, ocitest.Saved{Tags: []string{"x:1"}, Arch: "amd64",
		Layers: [][]byte{ocitest.Tar([]ocitest.File{{Name: "big", Body: strings.Repeat("x", 3<<20)}})}})
	viejo := fakeDaemon(t, (&blobsMux{cache: t.TempDir()}).handler())
	if _, err := abrirArchivoImport(ctx, viejo, archivo, "", "", 0); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("an old daemon: %v", err)
	}
	c := fakeDaemon(t, (&blobsMux{cache: t.TempDir(), caps: []string{api.CapabilityOCIBlobs}}).handler())
	if _, err := abrirArchivoImport(ctx, c, archivo, "", "", 2); err == nil || !strings.Contains(err.Error(), "over the 2 MiB limit") {
		t.Fatalf("-max-size: %v", err)
	}
	if _, err := abrirArchivoImport(ctx, c, archivo, "", "arm64", 0); err == nil || !strings.Contains(err.Error(), "not linux/arm64") {
		t.Fatalf("-arch: %v", err)
	}
	ai, err := abrirArchivoImport(ctx, c, archivo, "x:1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	ai.a.Close()
	if got := nombreArchivo(ai.img); got != "x-1" {
		t.Fatalf("name %q", got)
	}
	if got := nombreArchivo(&oci.ArchiveImage{ConfigDigest: "sha256:" + strings.Repeat("ab", 32)}); got != "archive-abababababab" {
		t.Fatalf("unnamed %q", got)
	}
}

func TestValidateOCISource(t *testing.T) {
	req := api.BuildImageRequest{Name: "x", Builder: "oci"}
	d := "sha256:" + strings.Repeat("a", 64)
	if _, err := validateOCI(req, OCISpec{Source: "http", Ref: "x", Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "invalid source") {
		t.Fatalf("source: %v", err)
	}
	if _, err := validateOCI(req, OCISpec{Source: ociSourceArchive, Ref: "x", Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("no digest: %v", err)
	}
	ref, err := validateOCI(req, OCISpec{Source: ociSourceArchive, Digest: d, Arch: "amd64"})
	if err != nil || ref.Digest != d {
		t.Fatalf("unnamed: %+v %v", ref, err)
	}
	if got := ociShown(OCISpec{Source: ociSourceArchive}, ref); got != "archive@"+d {
		t.Fatalf("shown %q", got)
	}
	// Con nombre, tal como venía en el archivo: no es de Docker Hub.
	if got := ociShown(OCISpec{Source: ociSourceArchive, Ref: "redis:7"}, ref); got != "redis:7@"+d {
		t.Fatalf("shown %q", got)
	}
}

// Una imagen del mismo archivo ya importada es la misma importación; de un
// registro, otra.
func TestExistingImportArchive(t *testing.T) {
	ctx := context.Background()
	d := "sha256:" + strings.Repeat("a", 64)
	c := fakeDaemon(t, fakeImagesMux("oci", `{"ref":"redis:7","digest":"`+d+`","arch":"amd64","restart":"on-failure","source":"archive"}`))
	mismo := OCISpec{Ref: "redis:7", Digest: d, Arch: "amd64", Restart: api.RestartOnFailure, Source: ociSourceArchive}
	if ok, err := existingImport(ctx, c, "redis-7", mismo); err != nil || !ok {
		t.Fatalf("the same archive import: %v %v", ok, err)
	}
	registro := OCISpec{Ref: "redis:7", Digest: d, Arch: "amd64", Restart: api.RestartOnFailure}
	if _, err := existingImport(ctx, c, "redis-7", registro); err == nil || !strings.Contains(err.Error(), "an archive (redis:7)") {
		t.Fatalf("from a registry over an archive import: %v", err)
	}
}

func TestArchiveAlreadyLine(t *testing.T) {
	for _, c := range [][2]string{{"dev", "dev"}, {"v0.17.0", "v0.18.0"}} {
		got := archiveAlreadyLine(alreadyImportedLine("pg", "the archive (postgres:17)", c[0], c[1]))
		if strings.Contains(got, "tag") || !strings.Contains(got, "-replace rebuilds it") {
			t.Fatalf("%q", got)
		}
	}
}
