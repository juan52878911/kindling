package guest

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// Quién puede conectar una carpeta: por TCP, cualquiera que no sea loopback
// ni la propia IP; por vsock, solo el host (CID 2).
func TestRequestFromGuest(t *testing.T) {
	pet := func(remota string, local net.Addr) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://x/share/attach", nil)
		r.RemoteAddr = remota
		return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
	}
	tcp := &net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 8080}
	vs := vsockAddr{cid: 0xFFFFFFFF, port: 8080}
	for _, c := range []struct {
		nombre string
		r      *http.Request
		dentro bool
	}{
		{"tcp del host", pet("172.16.0.1:5000", tcp), false},
		{"tcp loopback", pet("127.0.0.1:5000", tcp), true},
		{"tcp propia IP", pet("172.16.0.2:5000", tcp), true},
		{"vsock del host", pet(vsockAddr{cid: 2}.String(), vs), false},
		{"vsock loopback", pet(vsockAddr{cid: 1, port: 9}.String(), vs), true},
		{"vsock sin peer", pet(vsockAddr{}.String(), vs), true},
	} {
		if got := requestFromGuest(c.r); got != c.dentro {
			t.Errorf("%s: fromGuest = %v, want %v", c.nombre, got, c.dentro)
		}
	}
}
