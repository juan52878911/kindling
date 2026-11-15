package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Sin token (-no-auth), el gateway solo atiende a peticiones dirigidas a
// loopback y de un origen de loopback: un navegador al que una página hace
// hablar con 127.0.0.1 con otro nombre, o desde otro origen, recibe 403. Con
// token, Host y Origin dan igual (manda el token).
func TestSinTokenSoloLoopback(t *testing.T) {
	sock := mockDaemon(t, nil, nil)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	r := NewRouter([]RouterHost{{Name: "uno", GW: New(api.NewClient(sock), 5*time.Minute, false, 0, "")}})
	pide := func(h http.Handler, host, origin, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/mcp/_all", strings.NewReader(cuerpoInit))
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for nombre, h := range map[string]http.Handler{"gateway": gw.Handler(""), "router": r.Handler("")} {
		for _, c := range []struct {
			host, origin string
			want         int
		}{
			{"127.0.0.1:8080", "", http.StatusOK},
			{"localhost:8080", "http://localhost:3000", http.StatusOK},
			{"[::1]:8080", "", http.StatusOK},
			{"rebind.example:8080", "", http.StatusForbidden},
			{"127.0.0.1:8080", "http://evil.example", http.StatusForbidden},
			{"127.0.0.1:8080", "null", http.StatusForbidden},
		} {
			if got := pide(h, c.host, c.origin, ""); got != c.want {
				t.Errorf("%s: Host %q Origin %q: %d, quería %d", nombre, c.host, c.origin, got, c.want)
			}
		}
	}

	conToken := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	conToken.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana"}})
	if got := pide(conToken.Handler(""), "gw.example:8080", "http://app.example", "tok-ana"); got != http.StatusOK {
		t.Errorf("con token, otro Host: %d, quería 200", got)
	}
}
