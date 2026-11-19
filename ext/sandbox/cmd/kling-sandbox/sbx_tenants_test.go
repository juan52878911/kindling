package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// capturar ejecuta f y devuelve lo que escribió en stdout y stderr.
func capturar(t *testing.T, f func() error) (out, errOut string, err error) {
	t.Helper()
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	so, se := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	err = f()
	os.Stdout, os.Stderr = so, se
	wo.Close()
	we.Close()
	bo, _ := io.ReadAll(ro)
	be, _ := io.ReadAll(re)
	return string(bo), string(be), err
}

func TestSbxTenantYKeyHablanConLaAPI(t *testing.T) {
	rutas, cuerpos := frontalFalso(t)
	if _, _, err := capturar(t, func() error { return sbxTenant([]string{"add", "ana", "-admin", "-max_sandboxes", "5"}) }); err != nil {
		t.Fatal(err)
	}
	if (*rutas)[0] != "POST /v1/tenants" || (*cuerpos)[0]["name"] != "ana" || (*cuerpos)[0]["admin"] != true ||
		(*cuerpos)[0]["limits"].(map[string]any)["max_sandboxes"] != float64(5) {
		t.Fatalf("add: %v %v", *rutas, *cuerpos)
	}
	// set sin -admin no toca el rol; con topes los manda todos.
	if _, _, err := capturar(t, func() error { return sbxTenant([]string{"set", "ana", "-max_shells", "2"}) }); err != nil {
		t.Fatal(err)
	}
	if (*rutas)[1] != "PATCH /v1/tenants/ana" {
		t.Fatalf("set: %v", *rutas)
	}
	if _, ok := (*cuerpos)[1]["admin"]; ok {
		t.Errorf("set sin -admin manda admin: %v", (*cuerpos)[1])
	}
	if _, _, err := capturar(t, func() error { return sbxTenant([]string{"rm", "ana"}) }); err != nil || (*rutas)[2] != "DELETE /v1/tenants/ana" {
		t.Fatalf("rm: %v %v", err, *rutas)
	}
}

func TestSbxKeyImprimeLaClaveSolaEnStdout(t *testing.T) {
	rutas, cuerpos := frontalFalso(t)
	// frontalFalso contesta {"id":"h/abc",...}: sin "key" imprime vacío, que basta para ver el reparto de salidas.
	out, errOut, err := capturar(t, func() error { return sbxKey([]string{"rotate", "ana", "k1", "-grace", "5m"}) })
	if err != nil {
		t.Fatal(err)
	}
	if (*rutas)[0] != "POST /v1/tenants/ana/keys/k1/rotate" || (*cuerpos)[0]["grace_minutes"] != float64(5) {
		t.Fatalf("rotate: %v %v", *rutas, *cuerpos)
	}
	if strings.TrimSpace(out) != "" || !strings.Contains(errOut, "key id: h/abc") {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
	if _, _, err := capturar(t, func() error { return sbxKey([]string{"rm", "ana", "k1"}) }); err != nil || (*rutas)[1] != "DELETE /v1/tenants/ana/keys/k1" {
		t.Fatalf("rm: %v %v", err, *rutas)
	}
}
