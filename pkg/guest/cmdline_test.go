package guest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un host más nuevo que la imagen pasa kling.* que este agente no conoce: se
// apartan para avisar y lo conocido se sigue leyendo igual.
func TestBootParamsIgnoraLosDesconocidos(t *testing.T) {
	p := parseBootParams("console=ttyS0 kling.futuro=x kling.volume=/data kling.exec=1 kling.otro kling.volume=/pisado")
	if got := p.values[volumeBootParam]; got != "/data" {
		t.Fatalf("kling.volume = %q, quería el primero, /data", got)
	}
	if p.values[execBootParam] != "1" {
		t.Fatalf("kling.exec = %q", p.values[execBootParam])
	}
	if want := []string{"kling.futuro=x", "kling.otro"}; !reflect.DeepEqual(p.unknown, want) {
		t.Fatalf("desconocidos %q, quería %q", p.unknown, want)
	}
	if _, ok := p.values["kling.futuro"]; ok {
		t.Fatal("un parámetro desconocido acabó entre los conocidos")
	}
}

// Una opción de volumen que este agente no conoce no se pega al nombre del
// directorio (lo que hacía el puente anterior a ":ro": EACCES y pánico de PID
// 1) y el volumen se monta de solo lectura, que es lo que no rompe nada.
func TestVolumenConOpcionDesconocidaNoCambiaElPunto(t *testing.T) {
	got := parseVolumeSpecs("/data,/libs:ro,/cache:rw,/nuevo:ro:nosuid,/raro:algo")
	want := []VolumeSpec{
		{device: "/dev/vdc", mount: "/data"},
		{device: "/dev/vdd", mount: "/libs", readOnly: true},
		{device: "/dev/vde", mount: "/cache"},
		{device: "/dev/vdf", mount: "/nuevo", readOnly: true},
		{device: "/dev/vdg", mount: "/raro", readOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\ngot  %+v\nwant %+v", got, want)
	}
	if parseVolumeSpecs("") != nil {
		t.Fatal("sin kling.volume no hay volúmenes")
	}
}

// /healthz sigue contestando "ok" a quien no pide JSON (hosts anteriores,
// scripts) y, a quien lo pide, qué agente es y qué sabe hacer.
func TestHealthzConVersionYCapacidades(t *testing.T) {
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}, Name: "kling-bridge", Version: "v0.18.0",
		ExtraCaps: []string{api.GuestCapMCP}}
	mux := http.NewServeMux()
	a.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Body.String() != "ok\n" {
		t.Fatalf("sin Accept: %q", rr.Body.String())
	}

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Accept", "application/json")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var h api.GuestHealth
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatalf("con Accept JSON: %v (%q)", err, rr.Body.String())
	}
	if h.Status != "ok" || h.Agent != "kling-bridge" || h.Version != "v0.18.0" {
		t.Fatalf("%+v", h)
	}
	for _, c := range []string{api.GuestCapResync, api.GuestCapReady, api.GuestCapHooks, api.GuestCapMCP, api.GuestCapBootOpt} {
		if !slices.Contains(h.Caps, c) {
			t.Errorf("falta %q en %q", c, h.Caps)
		}
	}
	if !ExecEnabled() && slices.Contains(h.Caps, api.GuestCapExec) {
		t.Error("anuncia exec sin kling.exec=1")
	}
}
