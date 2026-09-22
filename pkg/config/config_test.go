package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enUnaConfigTemporal(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("KLING_CONFIG", p)
	t.Setenv("KLING_HOST", "")
	return p
}

// Aqui viven el endpoint del daemon y el token del gateway, que los pone una
// persona. La escritura pasa por internal/durable, y el fichero tiene que nacer
// con permisos restringidos: un 0644 dejaria el token legible para cualquier
// cuenta de la maquina.
func TestElFicheroDeConfiguracionNoQuedaLegibleParaTodos(t *testing.T) {
	p := enUnaConfigTemporal(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Set("gateway.token", "un-token-secreto-de-verdad"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("no se escribió: %v", err)
	}
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		t.Errorf("permisos %v: el token queda legible para otros", m)
	}
	// Y no queda un temporal tirado al lado con el mismo contenido.
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("quedó un .tmp junto a la configuración, con el token dentro")
	}
}

// Un fichero ausente NO es un error: la primera vez que alguien usa el CLI no
// hay configuración, y fallar ahí lo dejaría inservible.
func TestSinFicheroLaConfiguracionEsUtilizable(t *testing.T) {
	enUnaConfigTemporal(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load sin fichero devolvió error: %v", err)
	}
	if c == nil || c.Contexts == nil {
		t.Fatal("Load devolvió algo inutilizable")
	}
	if h := c.Host(""); h != "" {
		t.Errorf("Host sin nada configurado = %q, esperaba vacío", h)
	}
}

// El orden de precedencia es el que espera cualquiera que use una CLI: la
// bandera manda sobre el entorno, y el entorno sobre el fichero.
func TestHostRespetaBanderaEntornoYFichero(t *testing.T) {
	enUnaConfigTemporal(t)
	c, _ := Load()
	// El host vive en el contexto, no en las claves que acepta Set.
	c.CurrentContext = "pruebas"
	c.Contexts["pruebas"] = &Context{Host: "unix:///del/fichero.sock"}

	if got := c.Host(""); got != "unix:///del/fichero.sock" {
		t.Errorf("solo fichero: %q", got)
	}
	t.Setenv("KLING_HOST", "ssh://del-entorno")
	if got := c.Host(""); got != "ssh://del-entorno" {
		t.Errorf("el entorno debería ganar al fichero: %q", got)
	}
	if got := c.Host("ssh://de-la-bandera"); got != "ssh://de-la-bandera" {
		t.Errorf("la bandera debería ganar a todo: %q", got)
	}
}

// Un valor invalido tiene que RECHAZARSE, no guardarse a medias: una egress
// desconocida en la configuracion se arrastraria a cada microVM creada.
func TestSetRechazaLoQueNoEntiende(t *testing.T) {
	enUnaConfigTemporal(t)
	c, _ := Load()

	for _, mal := range [][2]string{
		{"sinpunto", "x"},
		{"defaults.egress", "internte"},
		{"defaults.mem_mib", "mucha"},
		{"seccion.desconocida", "x"},
	} {
		if err := c.Set(mal[0], mal[1]); err == nil {
			t.Errorf("Set(%q, %q) se aceptó", mal[0], mal[1])
		}
	}
	// Y lo válido sí entra.
	if err := c.Set("defaults.egress", "internet"); err != nil {
		t.Errorf("un valor válido se rechazó: %v", err)
	}
}

// El token no puede salir entero al listar la configuracion: `kling config` se
// pega en incidencias y en capturas de pantalla.
func TestElTokenSaleEnmascarado(t *testing.T) {
	enUnaConfigTemporal(t)
	c, _ := Load()
	secreto := "sk-1234567890abcdef"
	if err := c.Set("gateway.token", secreto); err != nil {
		t.Fatal(err)
	}
	for _, kv := range c.Keys() {
		if strings.Contains(kv[1], secreto) {
			t.Errorf("la clave %q muestra el token entero: %q", kv[0], kv[1])
		}
	}
	if m := mask(secreto); m == secreto || !strings.Contains(m, "…") {
		t.Errorf("mask(%q) = %q", secreto, m)
	}
	if m := mask("corto"); m != "********" {
		t.Errorf("un secreto corto debe ocultarse entero, no recortarse: %q", m)
	}
	if mask("") != "" {
		t.Error("mask de vacío debería ser vacío, no ocho asteriscos: parecería que hay algo")
	}
}
