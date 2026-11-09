package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gatewayQueAnota es un gateway falso que contesta al initialize y apunta qué
// cabeceras Authorization le llegan.
func gatewayQueAnota(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var vistos []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		vistos = append(vistos, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"eco"},"tools":[{"name":"echo"}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), vistos...)
	}
}

// TestAyudanteToken no es un test: es el proceso hijo de los de abajo, para
// poder ver qué pasa con `-token` aunque el FlagSet salga con os.Exit.
func TestAyudanteToken(t *testing.T) {
	raw := os.Getenv("KLINGMCP_AYUDANTE")
	if raw == "" {
		t.Skip("solo como proceso hijo")
	}
	var args []string
	_ = json.Unmarshal([]byte(raw), &args)
	var err error
	switch args[0] {
	case "connect":
		err = cmdConnect(args[1:])
	case "migrate":
		err = cmdMigrate(args[1:])
	}
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func ayudante(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAyudanteToken$")
	cmd.Env = append(os.Environ(), "KLINGMCP_AYUDANTE="+string(b),
		"KLING_CONFIG="+filepath.Join(t.TempDir(), "config.json"), "HOME="+t.TempDir())
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// `connect` y `migrate` no aceptan el token por argv: la línea de comandos la
// lee cualquiera en ps o /proc. El gateway ya lo prohibía; los clientes no.
func TestConnectYMigrateRechazanTokenEnArgv(t *testing.T) {
	srv, vistos := gatewayQueAnota(t)
	for _, sub := range []string{"connect", "migrate"} {
		out, err := ayudante(t, []string{"KLING_GATEWAY_TOKEN="}, sub, "-gateway", srv.URL, "-token", "secreto-argv", "eco")
		if err == nil {
			t.Fatalf("%s -token terminó bien:\n%s", sub, out)
		}
		if !strings.Contains(out, "KLING_GATEWAY_TOKEN") {
			t.Errorf("%s -token no dice por dónde pasar el token:\n%s", sub, out)
		}
	}
	for _, a := range vistos() {
		if strings.Contains(a, "secreto-argv") {
			t.Fatal("el token de argv llegó al gateway")
		}
	}
}

// El token llega por el entorno o por fichero, con prioridad sobre gateway.token.
func TestConnectLeeElTokenDelEntornoODeFichero(t *testing.T) {
	srv, vistos := gatewayQueAnota(t)
	if out, err := ayudante(t, []string{"KLING_GATEWAY_TOKEN=tok-env"}, "connect", "-gateway", srv.URL, "eco"); err != nil {
		t.Fatalf("connect con KLING_GATEWAY_TOKEN: %v\n%s", err, out)
	}
	f := filepath.Join(t.TempDir(), "gateway.env")
	if err := os.WriteFile(f, []byte("KLING_GATEWAY_TOKEN=tok-fichero\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := ayudante(t, []string{"KLING_GATEWAY_TOKEN=tok-env"}, "migrate", "-gateway", srv.URL, "-token-file", f, "eco"); err != nil {
		t.Fatalf("migrate -token-file: %v\n%s", err, out)
	}
	todo := strings.Join(vistos(), "\n")
	if !strings.Contains(todo, "Bearer tok-env") || !strings.Contains(todo, "Bearer tok-fichero") {
		t.Fatalf("cabeceras vistas:\n%s", todo)
	}
}
