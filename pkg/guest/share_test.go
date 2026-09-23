package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
	"github.com/juan52878911/kindling/pkg/share"
)

// attachar hace lo que el daemon: POST /share/attach con Upgrade y, si el
// agente acepta, sirve la carpeta por esa conexión.
func attachar(t *testing.T, url string, a share.Attach, srv *hostshare.Server) (int, func()) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(a)
	req, _ := http.NewRequest(http.MethodPost, url+share.AttachPath, bytes.NewReader(body))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", share.Proto)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return resp.StatusCode, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, struct {
			io.Reader
			io.Writer
			io.Closer
		}{br, conn, conn})
		close(done)
	}()
	return resp.StatusCode, func() { cancel(); conn.Close(); <-done }
}

func TestAttachMontaYReconecta(t *testing.T) {
	dir := carpeta(t)
	dev := newDevFalso()
	montajes := 0
	s := newShares()
	s.fromGuest = func(*http.Request) bool { return false }
	s.mount = func(target string, ro bool) (fuseDev, func(), error) {
		montajes++
		return dev, func() {}, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc(share.AttachPath, s.AttachHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()
	defer close(dev.in)

	srv, err := hostshare.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	a := share.Attach{Tag: 0, Mount: "/work", Mode: share.ModeRW}
	code, stop := attachar(t, ts.URL, a, srv)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("attach: %d", code)
	}
	if e, _ := dev.pedir(t, opInit, 0, u32s(7, 31, 0, 0)); e != 0 {
		t.Fatalf("init: %d", e)
	}
	if e, _ := dev.pedir(t, opLookup, rootID, cstr("hello.txt")); e != 0 {
		t.Fatalf("lookup through the attached session: %d", e)
	}

	// Otro attach con la misma carpeta (tras descongelar): no vuelve a montar.
	stop()
	code, stop = attachar(t, ts.URL, a, srv)
	defer stop()
	if code != http.StatusSwitchingProtocols || montajes != 1 {
		t.Fatalf("reattach: code %d, mounts %d", code, montajes)
	}
	if e, _ := dev.pedir(t, opLookup, rootID, cstr("sub")); e != 0 {
		t.Fatalf("lookup after reattach: %d", e)
	}

	// El mismo tag en otro sitio, u otro tag en el mismo sitio: 409.
	if code, _ := attachar(t, ts.URL, share.Attach{Tag: 0, Mount: "/other", Mode: share.ModeRW}, srv); code != http.StatusConflict {
		t.Errorf("same tag elsewhere = %d", code)
	}
	if code, _ := attachar(t, ts.URL, share.Attach{Tag: 1, Mount: "/work", Mode: share.ModeRW}, srv); code != http.StatusConflict {
		t.Errorf("same mount, other tag = %d", code)
	}
	// Peticiones inválidas.
	if code, _ := attachar(t, ts.URL, share.Attach{Tag: 9, Mount: "/x", Mode: share.ModeRW}, srv); code != http.StatusBadRequest {
		t.Errorf("tag out of range = %d", code)
	}
	if code, _ := attachar(t, ts.URL, share.Attach{Tag: 2, Mount: "/proc/x", Mode: share.ModeRW}, srv); code != http.StatusBadRequest {
		t.Errorf("mount over /proc = %d", code)
	}
	// Sin Upgrade: 426.
	resp, err := http.Post(ts.URL+share.AttachPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("without upgrade = %d", resp.StatusCode)
	}
}

// Desde dentro del invitado no se hace attach: vería lo que se escribe.
func TestAttachDesdeDentroSeRechaza(t *testing.T) {
	s := newShares()
	s.mount = func(string, bool) (fuseDev, func(), error) { t.Fatal("mounted"); return nil, nil, nil }
	mux := http.NewServeMux()
	mux.HandleFunc(share.AttachPath, s.AttachHandler())
	ts := httptest.NewServer(mux) // loopback: "desde dentro"
	defer ts.Close()
	srv, _ := hostshare.Open(t.TempDir(), true)
	defer srv.Close()
	if code, _ := attachar(t, ts.URL, share.Attach{Tag: 0, Mount: "/work", Mode: share.ModeRO}, srv); code != http.StatusForbidden {
		t.Fatalf("attach from loopback = %d, want 403", code)
	}
}

func TestAttachSinFUSE(t *testing.T) {
	s := newShares()
	s.mount = func(string, bool) (fuseDev, func(), error) { return nil, nil, errNoFUSE }
	if _, code, err := s.mountFor(share.Attach{Tag: 0, Mount: "/work", Mode: share.ModeRO}); err == nil || code != http.StatusNotImplemented {
		t.Fatalf("mount without FUSE: code %d err %v", code, err)
	}
}
