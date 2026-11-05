package guest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
	share "github.com/juan52878911/kindling/internal/shareproto"
)

// attachVirtio es el attach por virtio-fs: el agente monta y contesta sin
// tomar la conexión. Devuelve el código y lo que dice HeaderTransport.
func attachVirtio(t *testing.T, url string, a share.Attach) (int, string) {
	t.Helper()
	a.Transport = share.TransportVirtioFS
	body, _ := json.Marshal(a)
	req, _ := http.NewRequest(http.MethodPost, url+share.AttachPath, bytes.NewReader(body))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", share.Proto)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(share.HeaderTransport)
}

// Por virtio-fs el agente monta el dispositivo de la carpeta (una vez) y
// contesta 200; si no puede, 501 pidiendo FUSE. Una carpeta montada por un
// transporte no se monta por el otro.
func TestAttachVirtioFS(t *testing.T) {
	s := newShares()
	s.fromGuest = func(*http.Request) bool { return false }
	var montados []string
	falla := false
	s.mountVirtio = func(tag, target string, ro bool) (func(), error) {
		if falla {
			return nil, errNoFUSE
		}
		montados = append(montados, tag+" "+target)
		return func() {}, nil
	}
	tras := make(chan string, 4)
	s.afterVirtio = func(target string) { tras <- target }
	dev := newDevFalso()
	defer close(dev.in)
	s.mount = func(string, bool) (fuseDev, func(), error) { return dev, func() {}, nil }
	mux := http.NewServeMux()
	mux.HandleFunc(share.AttachPath, s.AttachHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	a := share.Attach{Tag: 3, Mount: "/app", Mode: share.ModeRW}
	if code, via := attachVirtio(t, ts.URL, a); code != http.StatusOK || via != share.TransportVirtioFS {
		t.Fatalf("virtio-fs attach: %d %q", code, via)
	}
	if len(montados) != 1 || montados[0] != "kling-share-3 /app" {
		t.Fatalf("mounted %v", montados)
	}
	if got := <-tras; got != "/app" {
		t.Fatalf("readahead for %q", got)
	}
	// Tras descongelar: no se vuelve a montar.
	if code, _ := attachVirtio(t, ts.URL, a); code != http.StatusOK || len(montados) != 1 {
		t.Fatalf("reattach: %d, %d mounts", code, len(montados))
	}
	// La misma carpeta pedida por FUSE: ya está por virtio-fs.
	srv, err := hostshare.Open(t.TempDir(), false, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if code, _ := attachar(t, ts.URL, a, srv); code != http.StatusConflict {
		t.Fatalf("FUSE attach of a virtio-fs share = %d", code)
	}
	// Otro tag en el mismo sitio: 409.
	if code, _ := attachVirtio(t, ts.URL, share.Attach{Tag: 4, Mount: "/app", Mode: share.ModeRW}); code != http.StatusConflict {
		t.Fatalf("same mount, other tag = %d", code)
	}
	// Sin virtio-fs (kernel sin él, VMM sin el dispositivo): 501 y "fuse".
	falla = true
	if code, via := attachVirtio(t, ts.URL, share.Attach{Tag: 5, Mount: "/b", Mode: share.ModeRO}); code != http.StatusNotImplemented || via != "fuse" {
		t.Fatalf("without virtio-fs: %d %q", code, via)
	}
	// Una montada por FUSE pedida por virtio-fs: 501 y "fuse", para que el
	// daemon siga por FUSE.
	b := share.Attach{Tag: 6, Mount: "/c", Mode: share.ModeRW}
	code, stop := attachar(t, ts.URL, b, srv)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("FUSE attach: %d", code)
	}
	defer stop()
	if code, via := attachVirtio(t, ts.URL, b); code != http.StatusNotImplemented || via != "fuse" {
		t.Fatalf("virtio-fs attach of a FUSE share: %d %q", code, via)
	}
	// Un transporte que no existe: 400.
	body, _ := json.Marshal(map[string]any{"tag": 7, "mount": "/d", "mode": "rw", "transport": "9p"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+share.AttachPath, bytes.NewReader(body))
	req.Header.Set("Upgrade", share.Proto)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown transport = %d", resp.StatusCode)
	}
}
