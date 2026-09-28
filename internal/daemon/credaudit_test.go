package daemon

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// GET /machines/{ref}/credaudit: NDJSON del registro del proxy, con la máquina
// congelada (es un fichero de su directorio), 400 con un parámetro que no se
// entiende y 404 con una máquina que no existe.
func TestHandleCredAudit(t *testing.T) {
	root := t.TempDir()
	seed := []*api.Machine{{ID: "c4c4c4c4c4c4c4c4c4", Name: "svc", State: api.StateWarm}}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	h := (&Server{mgr: mgr, root: root, bus: bus}).routes()

	dir := filepath.Join(root, "machines", "c4c4c4c4c4c4c4c4c4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Sin registro: 200 y vacío.
	if rr := call(t, h, "GET", "/machines/svc/credaudit", ""); rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatalf("sin registro: %d %q", rr.Code, rr.Body)
	}
	a := credproxy.NewAuditor(filepath.Join(dir, credproxy.AuditFile), nil)
	ahora := time.Now()
	a.Record(credproxy.Record{TS: ahora.Add(-time.Hour), Kind: credproxy.KindHTTP, Method: "GET", Host: "a.com", Path: "/viejo", Status: 200})
	a.Record(credproxy.Record{TS: ahora, Kind: credproxy.KindHTTP, Method: "GET", Host: "a.com", Path: "/no",
		Status: 403, Reason: credproxy.ReasonNoCredential, Denied: true})
	a.Record(credproxy.Record{TS: ahora, Kind: credproxy.KindHTTP, Method: "POST", Host: "a.com", Path: "/si", Status: 200})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	leer := func(q string) []api.CredAuditRecord {
		t.Helper()
		rr := call(t, h, "GET", "/machines/svc/credaudit"+q, "")
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("%s: %d %s", q, rr.Code, rr.Body)
		}
		var out []api.CredAuditRecord
		sc := bufio.NewScanner(rr.Body)
		for sc.Scan() {
			var r api.CredAuditRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	if got := leer(""); len(got) != 3 || got[0].Path != "/viejo" {
		t.Fatalf("todo: %+v", got)
	}
	if got := leer("?denied=1"); len(got) != 1 || got[0].Reason != "no_credential" {
		t.Fatalf("denied: %+v", got)
	}
	if got := leer("?since=10m"); len(got) != 2 {
		t.Fatalf("since=10m: %+v", got)
	}
	if got := leer("?since=" + url.QueryEscape(ahora.Add(-time.Minute).Format(time.RFC3339))); len(got) != 2 {
		t.Fatalf("since RFC 3339: %+v", got)
	}
	if got := leer("?tail=1"); len(got) != 1 || got[0].Path != "/si" {
		t.Fatalf("tail=1: %+v", got)
	}
	for _, q := range []string{"tail=abc", "tail=-1", "denied=quizá", "since=ayer", "since=-5m"} {
		if rr := call(t, h, "GET", "/machines/svc/credaudit?"+q, ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: quería 400, salió %d %s", q, rr.Code, rr.Body)
		}
	}
	if rr := call(t, h, "GET", "/machines/nadie/credaudit", ""); rr.Code != http.StatusNotFound ||
		!strings.Contains(rr.Body.String(), "does not exist") {
		t.Fatalf("máquina inexistente: %d %s", rr.Code, rr.Body)
	}
}
