package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func enUnaConfigTemporal(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("KLING_CONFIG", p)
	t.Setenv("KLING_HOST", "")
	t.Setenv("KLING_SOCKET", "")
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

// Un config.json con tokens del gateway que otros pueden leer (escrito a mano,
// copiado con cp o de antes de durable) avisa al cargarlo, como hace
// `kling ai` con su registro; sin tokens, o con 0600, calla.
func TestAvisoConfigConTokensLegible(t *testing.T) {
	p := enUnaConfigTemporal(t)
	var avisos bytes.Buffer
	viejo := Avisos
	Avisos = &avisos
	defer func() { Avisos = viejo }()

	escribir := func(contenido string, modo os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(contenido), modo); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, modo); err != nil {
			t.Fatal(err)
		}
		avisos.Reset()
		if _, err := Load(); err != nil {
			t.Fatal(err)
		}
	}
	escribir(`{"gateway":{"token":"secreto"}}`, 0o644)
	if !strings.Contains(avisos.String(), "chmod 600") {
		t.Errorf("token en un 0644 sin aviso: %q", avisos.String())
	}
	// Una sola vez por orden, aunque se cargue varias veces.
	avisos.Reset()
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if avisos.Len() != 0 {
		t.Errorf("aviso repetido: %q", avisos.String())
	}
	escribir(`{"gateway":{"tokens":[{"name":"a","token":"secreto"}]}}`, 0o640)
	if !strings.Contains(avisos.String(), "chmod 600") {
		t.Errorf("tokens en un 0640 sin aviso: %q", avisos.String())
	}
	escribir(`{"gateway":{"token":"secreto"}}`, 0o600)
	if avisos.Len() != 0 {
		t.Errorf("0600 con aviso: %q", avisos.String())
	}
	escribir(`{"defaults":{"image":"min"}}`, 0o644)
	if avisos.Len() != 0 {
		t.Errorf("sin tokens con aviso: %q", avisos.String())
	}
}

// KLING_SOCKET es el último escalón: el socket de un daemon privado
// (KLING_SOCKET=... kling daemon) lo encuentra el cliente con la misma
// variable, pero un contexto activo o KLING_HOST mandan sobre él.
func TestHostKlingSocketUltimoEscalon(t *testing.T) {
	enUnaConfigTemporal(t)
	t.Setenv("KLING_HOST", "")
	t.Setenv("KLING_SOCKET", "/run/privado/kling.sock")
	c, _ := Load()
	c.CurrentContext = ""
	if got := c.Host(""); got != "unix:///run/privado/kling.sock" {
		t.Errorf("solo KLING_SOCKET: %q", got)
	}
	t.Setenv("KLING_SOCKET", "unix:///run/privado/kling.sock")
	if got := c.Host(""); got != "unix:///run/privado/kling.sock" {
		t.Errorf("KLING_SOCKET con unix://: %q", got)
	}
	c.CurrentContext = "pruebas"
	c.Contexts["pruebas"] = &Context{Host: "unix:///del/fichero.sock"}
	if got := c.Host(""); got != "unix:///del/fichero.sock" {
		t.Errorf("el contexto debería ganar a KLING_SOCKET: %q", got)
	}
	t.Setenv("KLING_HOST", "ssh://del-entorno")
	if got := c.Host(""); got != "ssh://del-entorno" {
		t.Errorf("KLING_HOST debería ganar a KLING_SOCKET: %q", got)
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

// defaults.* numéricos: 0 es «el de siempre» (como daemon.*), un negativo o un
// valor absurdo se rechaza en Set en vez de llegar a cada `kling run`.
func TestSetDefaultsNumericosEnRango(t *testing.T) {
	enUnaConfigTemporal(t)
	c, _ := Load()
	for _, mal := range [][2]string{
		{"defaults.mem_mib", "-1"},
		{"defaults.mem_mib", "8"},
		{"defaults.mem_mib", "2000000"},
		{"defaults.vcpus", "-2"},
		{"defaults.vcpus", "1000"},
		{"defaults.cpu_pct", "-50"},
		{"defaults.cpu_pct", "30000"},
		{"defaults.ttl_seconds", "-1"},
		{"defaults.ttl_seconds", "99999999999"},
	} {
		if err := c.Set(mal[0], mal[1]); err == nil {
			t.Errorf("Set(%q, %q) se aceptó", mal[0], mal[1])
		}
	}
	if c.Defaults.MemMiB != 0 || c.Defaults.VCPUs != 0 || c.Defaults.CPUPct != 0 || c.Defaults.TTL != 0 {
		t.Errorf("un valor rechazado quedó guardado: %+v", c.Defaults)
	}
	for _, bien := range [][2]string{
		{"defaults.mem_mib", "0"}, {"defaults.mem_mib", "512"},
		{"defaults.vcpus", "0"}, {"defaults.vcpus", "4"},
		{"defaults.cpu_pct", "0"}, {"defaults.cpu_pct", "200"},
		{"defaults.ttl_seconds", "0"}, {"defaults.ttl_seconds", "3600"},
	} {
		if err := c.Set(bien[0], bien[1]); err != nil {
			t.Errorf("Set(%q, %q): %v", bien[0], bien[1], err)
		}
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

func TestExtensiones(t *testing.T) {
	c := &Config{}
	if err := c.SetExtension("mcp", "memory.enabled", "bool", "sí"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExtension("mcp", "port", "int", "8080"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExtension("mcp", "token", "secret", "abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	var on bool
	if ok, err := c.Extension("mcp", "memory.enabled", &on); !ok || err != nil || !on {
		t.Fatalf("bool: ok=%v err=%v on=%v", ok, err, on)
	}
	var port int
	if ok, _ := c.Extension("mcp", "port", &port); !ok || port != 8080 {
		t.Fatalf("int: %d", port)
	}
	if v := c.ExtensionValue("mcp", "token", "secret"); v == "abcdefghijklmnop" || v == "" {
		t.Fatalf("un secreto debe mostrarse enmascarado: %q", v)
	}
	if err := c.SetExtension("mcp", "port", "int", "ocho"); err == nil {
		t.Fatal("un int que no es número debe rechazarse")
	}
	if err := c.SetExtension("mcp", "memory.enabled", "bool", "quizá"); err == nil {
		t.Fatal("un bool ambiguo debe rechazarse")
	}
	if ok, _ := c.Extension("otra", "k", new(string)); ok {
		t.Fatal("una clave nunca puesta no existe")
	}
}

// La sección "memory" de v0.5 era de kindling-mcp: se eleva a extensions.mcp al
// cargar y no se vuelve a escribir.
func TestSeElevaLaMemoriaAntigua(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.json"
	t.Setenv("KLING_CONFIG", p)
	if err := os.WriteFile(p, []byte(`{"memory":{"enabled":true,"service":"engram"},"defaults":{"image":"min"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var on bool
	var svc string
	if ok, _ := c.Extension("mcp", "memory.enabled", &on); !ok || !on {
		t.Fatal("memory.enabled no se elevó a extensions.mcp")
	}
	if ok, _ := c.Extension("mcp", "memory.service", &svc); !ok || svc != "engram" {
		t.Fatalf("memory.service no se elevó: %q", svc)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), `"memory"`) {
		t.Fatalf("la sección antigua no debe volver a escribirse: %s", b)
	}
	if !strings.Contains(string(b), `"image": "min"`) {
		t.Fatalf("se perdió el resto de la configuración: %s", b)
	}
}

func TestValidateVMM(t *testing.T) {
	casos := []struct {
		name, goos, goarch string
		ok                 bool
	}{
		{"firecracker", "linux", "amd64", true},
		{"firecracker", "linux", "arm64", true},
		{"firecracker", "darwin", "arm64", false},
		{"vz", "darwin", "arm64", true},
		{"vz", "darwin", "amd64", false},
		{"vz", "linux", "arm64", false},
		{"hv", "darwin", "arm64", true},
		{"hv", "darwin", "amd64", false},
		{"hv", "linux", "arm64", false},
		{"auto", "darwin", "arm64", true},
		{"auto", "linux", "amd64", false},
		{"qemu", "linux", "amd64", false},
	}
	for _, c := range casos {
		err := ValidateVMM(c.name, c.goos, c.goarch)
		if (err == nil) != c.ok {
			t.Errorf("ValidateVMM(%s, %s/%s) = %v", c.name, c.goos, c.goarch, err)
		}
	}
	if DefaultVMM("darwin") != VMMAuto || DefaultVMM("linux") != VMMFirecracker {
		t.Fatal("DefaultVMM")
	}
}

func TestSetDaemonVMM(t *testing.T) {
	c := &Config{}
	if err := c.Set("daemon.vmm", "qemu"); err == nil {
		t.Fatal("un backend desconocido no se acepta")
	}
	nativo := DefaultVMM(runtime.GOOS)
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("vz solo en Apple Silicon")
	}
	if err := c.Set("daemon.vmm", nativo); err != nil {
		t.Fatalf("el backend de la plataforma se acepta: %v", err)
	}
	found := false
	for _, kv := range c.Keys() {
		if kv[0] == "daemon.vmm" && kv[1] == nativo {
			found = true
		}
	}
	if !found {
		t.Fatal("daemon.vmm no aparece en Keys")
	}
	if err := c.Set("daemon.vmm", ""); err != nil || c.Daemon.VMM != "" {
		t.Fatal("vaciarlo vuelve al defecto")
	}
}

func TestSetDaemonCoW(t *testing.T) {
	c := &Config{}
	if err := c.Set("daemon.cow", "siempre"); err == nil {
		t.Fatal("un modo desconocido no se acepta")
	}
	for _, v := range []string{CoWAuto, CoWStore, CoWOff, ""} {
		if err := c.Set("daemon.cow", v); err != nil || c.Daemon.CoW != v {
			t.Fatalf("%q: %v", v, err)
		}
	}
	if err := c.Set("daemon.cow_store_gib", "-1"); err == nil {
		t.Fatal("un tamaño negativo no se acepta")
	}
	if err := c.Set("daemon.cow_store_gib", "32"); err != nil || c.Daemon.CoWStoreGiB != 32 {
		t.Fatalf("32: %v", err)
	}
	claves := map[string]bool{}
	for _, kv := range c.Keys() {
		claves[kv[0]] = true
	}
	if !claves["daemon.cow"] || !claves["daemon.cow_store_gib"] {
		t.Fatal("daemon.cow o daemon.cow_store_gib no aparecen en Keys")
	}
}

// plugins.disabled sobrevive a Save/Load y no se duplica al desactivar dos
// veces: es lo que editan `kling plugins enable|disable`.
func TestPluginsDesactivadosSeGuardan(t *testing.T) {
	enUnaConfigTemporal(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.SetPluginDisabled("mcp", true) || !c.SetPluginDisabled("ai", true) {
		t.Fatal("desactivar la primera vez tiene que cambiar algo")
	}
	if c.SetPluginDisabled("mcp", true) {
		t.Fatal("desactivar dos veces no cambia nada")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Plugins.Disabled, ","); got != "ai,mcp" {
		t.Fatalf("plugins.disabled = %q", got)
	}
	if !c.PluginDisabled("ai") || c.PluginDisabled("sandbox") {
		t.Fatal("PluginDisabled no refleja lo guardado")
	}
	if !c.SetPluginDisabled("ai", false) || !c.SetPluginDisabled("mcp", false) {
		t.Fatal("activar tiene que cambiar algo")
	}
	if c.Plugins.Disabled != nil {
		t.Fatalf("sin desactivadas la lista queda vacía: %q", c.Plugins.Disabled)
	}
}

func TestSetCredAudit(t *testing.T) {
	var c Config
	if err := c.Set("daemon.credaudit_max_mib", "16"); err != nil || c.Daemon.CredAuditMiB != 16 {
		t.Fatalf("%v %d", err, c.Daemon.CredAuditMiB)
	}
	if err := c.Set("daemon.credaudit_generations", "5"); err != nil || c.Daemon.CredAuditGenerations != 5 {
		t.Fatalf("%v %d", err, c.Daemon.CredAuditGenerations)
	}
	for _, kv := range [][2]string{{"daemon.credaudit_max_mib", "-1"}, {"daemon.credaudit_max_mib", "2000"}, {"daemon.credaudit_generations", "100"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%s aceptado", kv[0], kv[1])
		}
	}
}

func TestSetBuildCache(t *testing.T) {
	var c Config
	if err := c.Set("daemon.build_cache_max_gib", "50"); err != nil || c.Daemon.BuildCacheMaxGiB != 50 {
		t.Fatalf("%v %d", err, c.Daemon.BuildCacheMaxGiB)
	}
	if err := c.Set("daemon.build_cache_max_days", "7"); err != nil || c.Daemon.BuildCacheMaxDays != 7 {
		t.Fatalf("%v %d", err, c.Daemon.BuildCacheMaxDays)
	}
	for _, kv := range [][2]string{{"daemon.build_cache_max_gib", "-1"}, {"daemon.build_cache_max_days", "x"}, {"daemon.build_cache_max_days", "36501"}, {"daemon.build_cache_max_gib", "1048577"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%s aceptado", kv[0], kv[1])
		}
	}
}

func TestSetBattery(t *testing.T) {
	var c Config
	for _, v := range []string{"auto", "on", "off", ""} {
		if err := c.Set("daemon.battery_mode", v); err != nil || c.Daemon.BatteryMode != v {
			t.Fatalf("battery_mode %q: %v (%q)", v, err, c.Daemon.BatteryMode)
		}
	}
	if err := c.Set("daemon.battery_mode", "siempre"); err == nil {
		t.Fatal("aceptó un modo que no existe")
	}
	if err := c.Set("daemon.battery_pause_after_seconds", "-1"); err != nil || c.Daemon.BatteryPauseAfter != -1 {
		t.Fatalf("-1: %v", err)
	}
	for _, v := range []string{"-2", "86401", "x"} {
		if err := c.Set("daemon.battery_pause_after_seconds", v); err == nil {
			t.Fatalf("aceptó battery_pause_after_seconds %q", v)
		}
	}
	visto := 0
	for _, kv := range c.Keys() {
		if kv[0] == "daemon.battery_mode" || kv[0] == "daemon.battery_pause_after_seconds" {
			visto++
		}
	}
	if visto != 2 {
		t.Fatal("las claves del modo batería no salen en Keys")
	}
}

func TestSetNetMiBps(t *testing.T) {
	var c Config
	if err := c.Set("daemon.net_mibps", "256"); err != nil || c.Daemon.NetMiBps != 256 {
		t.Fatalf("256: %v (%d)", err, c.Daemon.NetMiBps)
	}
	for _, v := range []string{"-1", "10241", "x"} {
		if err := c.Set("daemon.net_mibps", v); err == nil {
			t.Fatalf("aceptó net_mibps %q", v)
		}
	}
	for _, kv := range c.Keys() {
		if kv[0] == "daemon.net_mibps" {
			if kv[1] != "256" {
				t.Fatalf("Keys: %q", kv[1])
			}
			return
		}
	}
	t.Fatal("daemon.net_mibps no sale en Keys")
}

// daemon.memory_pressure: auto (o vacío) u off.
func TestSetMemoryPressure(t *testing.T) {
	var c Config
	for _, v := range []string{"auto", "off", ""} {
		if err := c.Set("daemon.memory_pressure", v); err != nil || c.Daemon.MemoryPressure != v {
			t.Fatalf("memory_pressure %q: %v", v, err)
		}
	}
	if err := c.Set("daemon.memory_pressure", "on"); err == nil {
		t.Fatal("aceptó memory_pressure on")
	}
	if !slices.ContainsFunc(c.Keys(), func(kv [2]string) bool { return kv[0] == "daemon.memory_pressure" }) {
		t.Fatal("daemon.memory_pressure no sale en Keys")
	}
}

// daemon.idle_freeze: 0 o una duración de al menos un minuto; vacío es 10m
// en el Mac y nunca en Linux.
func TestSetIdleFreeze(t *testing.T) {
	var c Config
	for _, v := range []string{"10m", "1h", "0", ""} {
		if err := c.Set("daemon.idle_freeze", v); err != nil || c.Daemon.IdleFreeze != v {
			t.Fatalf("idle_freeze %q: %v", v, err)
		}
	}
	for _, v := range []string{"5s", "x", "-1m"} {
		if err := c.Set("daemon.idle_freeze", v); err == nil {
			t.Fatalf("aceptó idle_freeze %q", v)
		}
	}
	if d, _ := ParseIdleFreeze("", "darwin"); d != 10*time.Minute {
		t.Fatalf("Mac: %v", d)
	}
	if d, _ := ParseIdleFreeze("", "linux"); d != 0 {
		t.Fatalf("Linux: %v", d)
	}
	if !slices.ContainsFunc(c.Keys(), func(kv [2]string) bool { return kv[0] == "daemon.idle_freeze" }) {
		t.Fatal("daemon.idle_freeze no sale en Keys")
	}
}

func TestSetDaemonCuotaCarpetas(t *testing.T) {
	c := &Config{}
	for _, k := range []string{"share_max_mib", "share_max_files", "share_min_free_mib"} {
		if err := c.Set("daemon."+k, "-2"); err == nil {
			t.Errorf("daemon.%s -2 se acepta", k)
		}
		if err := c.Set("daemon."+k, "-1"); err != nil {
			t.Errorf("daemon.%s -1 (sin tope): %v", k, err)
		}
	}
	if err := c.Set("daemon.share_max_mib", "4096"); err != nil || c.Daemon.ShareMaxMiB != 4096 {
		t.Fatalf("share_max_mib 4096: %v (%d)", err, c.Daemon.ShareMaxMiB)
	}
	if c.Daemon.ShareMaxFiles != -1 || c.Daemon.ShareMinFreeMiB != -1 {
		t.Fatalf("files %d, min free %d", c.Daemon.ShareMaxFiles, c.Daemon.ShareMinFreeMiB)
	}
}
