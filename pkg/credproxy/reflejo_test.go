package credproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// formasDe son transformaciones que un proveedor que refleja (un LLM al que
// se le pide "repite esto en base64 / con espacios") puede aplicar a lo que
// recibió. Las que el redactor reconoce y la que no (con espacios).
func formasDe(s string) map[string]string {
	b := []byte(s)
	return map[string]string{
		"base64":           base64.StdEncoding.EncodeToString(b),
		"base64url":        base64.RawURLEncoding.EncodeToString(b),
		"base64 en medio":  base64.StdEncoding.EncodeToString(append([]byte("k="), b...)),
		"hex":              hex.EncodeToString(b),
		"HEX":              strings.ToUpper(hex.EncodeToString(b)),
		"mayúsculas":       strings.ToUpper(s),
		"ruta escapada":    url.PathEscape(s),
		"userinfo":         url.User(s).String(),
		"con espacios":     strings.Join(strings.Split(s, ""), " "),
		"base64 espaciado": strings.Join(strings.Split(base64.StdEncoding.EncodeToString(b), ""), " "),
	}
}

// reflejo es un proveedor que contesta con todas las formasDe del cuerpo que
// recibió, una por línea, y guarda ese cuerpo.
func reflejo(recibido *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*recibido = string(b)
		for _, f := range formasDe(string(b)) {
			io.WriteString(w, f+"\n")
		}
	}
}

func pedirConCuerpo(t *testing.T, url, cuerpo string) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat", strings.NewReader(cuerpo))
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

// Por defecto el marcador NO se cambia en el cuerpo: el proveedor que lo
// refleja (en base64, con espacios...) devuelve el marcador, nunca la clave.
func TestProxyPorDefectoNoSustituyeEnElCuerpo(t *testing.T) {
	var recibido string
	srv, _ := proxyCon(t, reflejo(&recibido), []Credential{
		{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret},
	}, nil)
	body := pedirConCuerpo(t, srv.URL, testPlace)
	if recibido != testPlace {
		t.Fatalf("el proveedor recibió %q en el cuerpo; quería el marcador tal cual", recibido)
	}
	for nombre, f := range formasDe(testSecret) {
		if bytes.Contains(body, []byte(f)) {
			t.Errorf("la clave volvió al invitado (%s): %s", nombre, body)
		}
	}
}

// Con Body la clave sí va en el cuerpo, y el redactor reconoce las formas más
// comunes en que puede volver. Con espacios no (y no se promete).
func TestProxyConBodyRedactaLasFormasComunes(t *testing.T) {
	var recibido string
	srv, _ := proxyCon(t, reflejo(&recibido), []Credential{
		{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret, Body: true},
	}, nil)
	body := pedirConCuerpo(t, srv.URL, testPlace)
	if recibido != testSecret {
		t.Fatalf("el proveedor recibió %q; con Body quería la clave", recibido)
	}
	for nombre, f := range formasDe(testSecret) {
		if strings.Contains(nombre, "espac") {
			continue
		}
		if bytes.Contains(body, []byte(f)) {
			t.Errorf("la clave volvió al invitado (%s): %s", nombre, body)
		}
	}
}

// Solo Authorization, X-Api-Key y las cabeceras que declare la credencial
// llevan la clave; en la query, solo con Query.
func TestProxySustituyeSoloEnLasCabecerasDeclaradas(t *testing.T) {
	var got http.Header
	var query string
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		got, query = r.Header.Clone(), r.URL.RawQuery
	}, []Credential{
		{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret, Headers: []string{"x-goog-api-key"}},
	}, nil)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/x?key="+testPlace, nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	req.Header.Set("X-Api-Key", testPlace)
	req.Header.Set("X-Goog-Api-Key", testPlace)
	req.Header.Set("X-Echo-Me", testPlace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("Authorization") != "Bearer "+testSecret || got.Get("X-Api-Key") != testSecret || got.Get("X-Goog-Api-Key") != testSecret {
		t.Errorf("cabeceras con clave: %v", got)
	}
	if got.Get("X-Echo-Me") != testPlace {
		t.Errorf("una cabecera no declarada llevó %q", got.Get("X-Echo-Me"))
	}
	if query != "key="+testPlace {
		t.Errorf("sin Query la query llevó %q", query)
	}
}

func TestValidarCabeceras(t *testing.T) {
	c := []Credential{{Domain: "a.example.com", Placeholder: testPlace, Secret: testSecret, Headers: []string{"x-goog-api-key"}}}
	if err := ValidarCredenciales(c); err != nil || c[0].Headers[0] != "X-Goog-Api-Key" {
		t.Fatalf("err %v, headers %v", err, c[0].Headers)
	}
	for _, h := range []string{"Host", "content-length", "X Bad", "", "Connection"} {
		c := []Credential{{Domain: "a.example.com", Placeholder: testPlace, Secret: testSecret, Headers: []string{h}}}
		if err := ValidarCredenciales(c); err == nil {
			t.Errorf("cabecera %q aceptada", h)
		}
	}
}
