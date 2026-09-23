package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// daemonFalso sirve lo justo del API para CopyImage: /info, /images y los
// blobs, guardados en memoria. Escucha en un socket unix, como el de verdad.
type daemonFalso struct {
	mu    sync.Mutex
	arch  string
	caps  []string
	imgs  []Image
	blobs map[string][]byte // "nombre/parte"
	puts  []string
}

func clave(name, part string) string {
	if name == KernelBlobName {
		part = BlobKernel
	}
	return name + "/" + part
}

func (d *daemonFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.URL.Path == "/info":
		_ = json.NewEncoder(w).Encode(Info{Version: "0.9.0", Arch: d.arch, Capabilities: d.caps})
	case r.URL.Path == "/images":
		_ = json.NewEncoder(w).Encode(d.imgs)
	case strings.HasSuffix(r.URL.Path, "/blob"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/blob")
		part := r.URL.Query().Get("part")
		if part == "" && name != KernelBlobName {
			for _, p := range []string{BlobImage, BlobLayer} {
				if _, ok := d.blobs[clave(name, p)]; ok {
					part = p
				}
			}
		}
		k := clave(name, part)
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			b, ok := d.blobs[k]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			h := sha256.Sum256(b)
			w.Header().Set(HeaderSha256, hex.EncodeToString(h[:]))
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(b)
			}
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			h := sha256.Sum256(b)
			if want := r.Header.Get(HeaderSha256); want != hex.EncodeToString(h[:]) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(Error{Message: "bad sha"})
				return
			}
			d.blobs[k] = b
			d.puts = append(d.puts, k)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(BlobPutResult{Name: name, Part: part})
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func servirFalso(t *testing.T, d *daemonFalso) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: d}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewClient("unix://" + sock)
}

func TestPlanCopiaCapaConBase(t *testing.T) {
	imgs := []Image{
		{Name: "min"},
		{Name: "svc", Base: "min", HasRecipe: true},
	}
	plan, err := planCopia(imgs, "svc")
	if err != nil {
		t.Fatal(err)
	}
	want := []blobRef{{KernelBlobName, BlobKernel}, {"min", BlobImage}, {"svc", BlobRecipe}, {"svc", BlobLayer}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %v", plan)
	}
	if _, err := planCopia([]Image{{Name: "svc", Base: "falta"}}, "svc"); err == nil || !strings.Contains(err.Error(), "falta") {
		t.Fatalf("base ausente: %v", err)
	}
	if _, err := planCopia(imgs, "nada"); err == nil {
		t.Fatal("imagen ausente")
	}
}

func TestCopyImageMueveCapaBaseYKernel(t *testing.T) {
	caps := []string{"image-blobs"}
	src := &daemonFalso{arch: "arm64", caps: caps,
		imgs: []Image{{Name: "min"}, {Name: "svc", Base: "min", HasRecipe: true}},
		blobs: map[string][]byte{
			"vmlinux/kernel": []byte("kernel"),
			"min/image":      []byte("base"),
			"svc/layer":      []byte("capa"),
			"svc/recipe":     []byte(`{"base":"min"}`),
		}}
	// El destino ya tiene el kernel idéntico: no se vuelve a mandar.
	dst := &daemonFalso{arch: "arm64", caps: caps, blobs: map[string][]byte{"vmlinux/kernel": []byte("kernel")}}
	cs, cd := servirFalso(t, src), servirFalso(t, dst)

	var pasos []CopyStep
	if err := CopyImage(context.Background(), cs, cd, "svc", func(p CopyStep) { pasos = append(pasos, p) }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"min/image", "svc/recipe", "svc/layer"}; !reflect.DeepEqual(dst.puts, want) {
		t.Fatalf("PUTs = %v, quiero %v", dst.puts, want)
	}
	if len(pasos) != 4 || !pasos[0].Skipped || pasos[1].Size != 4 {
		t.Fatalf("pasos = %+v", pasos)
	}
	for k, v := range src.blobs {
		if string(dst.blobs[k]) != string(v) {
			t.Errorf("%s: %q en destino", k, dst.blobs[k])
		}
	}
}

func TestCopyImageRechazaOtraArquitecturaYDaemonViejo(t *testing.T) {
	src := &daemonFalso{arch: "amd64", caps: []string{"image-blobs"}, imgs: []Image{{Name: "min"}}}
	dst := &daemonFalso{arch: "arm64", caps: []string{"image-blobs"}}
	err := CopyImage(context.Background(), servirFalso(t, src), servirFalso(t, dst), "min", nil)
	if err == nil || !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("arquitecturas distintas: %v", err)
	}
	viejo := &daemonFalso{arch: "arm64"}
	err = CopyImage(context.Background(), servirFalso(t, viejo), servirFalso(t, dst), "min", nil)
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("daemon sin image-blobs: %v", err)
	}
}
