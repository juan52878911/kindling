package fc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestKlingForwardsDevuelveLaTraduccion(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/kling/forwards" {
			t.Errorf("petición inesperada %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"forwards":{"8080":"127.0.0.1:61234","9000":"127.0.0.1:61235"}}`)
	})
	fwd, err := c.KlingForwards(context.Background(), []int{8080, 9000})
	if err != nil {
		t.Fatal(err)
	}
	if fwd["8080"] != "127.0.0.1:61234" || fwd["9000"] != "127.0.0.1:61235" {
		t.Fatalf("forwards = %v", fwd)
	}
}

func TestKlingForwardsIncompletoEsError(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"forwards":{"8080":"127.0.0.1:61234"}}`)
	})
	if _, err := c.KlingForwards(context.Background(), []int{8080, 9000}); err == nil ||
		!strings.Contains(err.Error(), "9000") {
		t.Fatalf("un puerto sin reenvío tiene que ser error, es %v", err)
	}
}

func TestSetKlingNetworkMandaLaPolitica(t *testing.T) {
	var cuerpo KlingNetwork
	c2, _, raw := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kling/network" {
			t.Errorf("ruta %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c2.SetKlingNetwork(context.Background(), KlingNetwork{Egress: "allowlist", AllowDomains: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(*raw, &cuerpo); err != nil || cuerpo.Egress != "allowlist" || cuerpo.AllowDomains[0] != "example.com" {
		t.Fatalf("cuerpo = %s (%v)", *raw, err)
	}
}

func TestKlingStatsEInfo(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kling/stats":
			_, _ = io.WriteString(w, `{"footprint_mib": 412}`)
		case "/kling/info":
			_, _ = io.WriteString(w, `{"backend":"vz","version":"0.1.0"}`)
		case "/kling/probe":
			fmt.Fprintf(w, `{"open": %t}`, r.URL.Query().Get("port") == "8080")
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"fault_message":"kling-vz does not implement this"}`)
		}
	})
	if n, err := c.KlingFootprintMiB(context.Background()); err != nil || n != 412 {
		t.Fatalf("footprint = %d, %v", n, err)
	}
	if i, err := c.KlingInfo(context.Background()); err != nil || i.Backend != "vz" {
		t.Fatalf("info = %+v, %v", i, err)
	}
	if open, err := c.KlingProbe(context.Background(), 8080); err != nil || !open {
		t.Fatalf("probe 8080 = %v, %v", open, err)
	}
	if open, err := c.KlingProbe(context.Background(), 9000); err != nil || open {
		t.Fatalf("probe 9000 = %v, %v", open, err)
	}
}

// Un Firecracker de verdad no conoce las rutas propias: contesta 400, y el
// error tiene que decirlo en vez de quedarse en un JSON ilegible.
func TestKlingRutaDesconocidaEsError(t *testing.T) {
	c, _, _ := firecrackerFalso(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"fault_message":"Invalid request method and/or path"}`)
	})
	_, err := c.KlingFootprintMiB(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid request") {
		t.Fatalf("err = %v", err)
	}
}
