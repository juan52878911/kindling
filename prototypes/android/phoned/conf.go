package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Rutas fijas dentro de la VM. Son las del lanzador de bash: android-sh y
// uidump (que siguen en la imagen para el camino con allow_exec) encuentran a
// Android por unshare.pid, y fase0.sh lee state.
const (
	confPath     = "/usr/local/lib/kindling-android/android.conf"
	argsPath     = "/usr/local/lib/kindling-android/entrypoint.args"
	checkPath    = "/usr/local/lib/kindling-android/check-android-config.sh"
	dataImgPath  = "/usr/local/lib/kindling-android/data.ext4"
	stateDir     = "/run/kindling-android"
	statePath    = stateDir + "/state"
	parentPIDPth = stateDir + "/unshare.pid"
	verityTable  = "/etc/kindling-android/layer.verity"
	prepMark     = stateDir + "/prepared"
)

// config es android.conf (lo escribe build-image.sh) más lo que se puede
// cambiar por el entorno, como en el lanzador.
type config struct {
	Root      string
	Width     string
	Height    string
	DPI       string
	FPS       string
	DataMode  string // overlay | tmpfs
	DataSize  string
	Net       string // veth | isolated | shared
	ExtraArgs []string
	// AdbSecure pide ro.adb.secure=1 a /init: adb con claves (las de cada clon
	// llegan por MMDS, identity.go).
	AdbSecure bool
	// Prep deja la pantalla encendida, sin bloqueo ni animaciones tras el
	// primer boot_completed (launch_linux.go, prepScript).
	Prep bool

	Ports       []int  // puertos de la VM que se reenvían a Android (adb)
	VethHost    string // la VM en el enlace veth
	VethAndroid string // Android en el enlace veth
	Listen      string // la API del teléfono
}

func defaultConfig() config {
	return config{
		Root: "/android", Width: "720", Height: "1280", DPI: "320", FPS: "15",
		DataMode: "overlay", DataSize: "2G", Net: "veth",
		Ports:    []int{5555},
		VethHost: "10.88.0.1", VethAndroid: "10.88.0.2",
		Listen: ":8091",
	}
}

// loadConfig lee android.conf y el entorno.
func loadConfig() (config, error) {
	c := defaultConfig()
	f, err := os.Open(confPath)
	if err != nil {
		return c, err
	}
	defer f.Close()
	kv, err := parseShellConf(f)
	if err != nil {
		return c, err
	}
	for _, k := range []string{"ANDROID_PORTS", "ANDROID_VETH_HOST", "ANDROID_VETH_ANDROID", "PHONED_LISTEN"} {
		if v, ok := os.LookupEnv(k); ok {
			kv[k] = v
		}
	}
	return c, c.apply(kv)
}

func (c *config) apply(kv map[string]string) error {
	set := func(dst *string, k string) {
		if v, ok := kv[k]; ok && v != "" {
			*dst = v
		}
	}
	set(&c.Root, "ANDROID_ROOT")
	set(&c.Width, "ANDROID_WIDTH")
	set(&c.Height, "ANDROID_HEIGHT")
	set(&c.DPI, "ANDROID_DPI")
	set(&c.FPS, "ANDROID_FPS")
	set(&c.DataMode, "ANDROID_DATA_MODE")
	set(&c.DataSize, "ANDROID_DATA_SIZE")
	set(&c.Net, "ANDROID_NET")
	set(&c.VethHost, "ANDROID_VETH_HOST")
	set(&c.VethAndroid, "ANDROID_VETH_ANDROID")
	set(&c.Listen, "PHONED_LISTEN")
	c.ExtraArgs = strings.Fields(kv["ANDROID_EXTRA_ARGS"])
	c.AdbSecure = kv["ANDROID_ADB_SECURE"] == "1"
	c.Prep = kv["ANDROID_PREP"] == "1"
	if v, ok := kv["ANDROID_PORTS"]; ok {
		c.Ports = nil
		for _, p := range strings.Fields(v) {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("ANDROID_PORTS: bad port %q", p)
			}
			c.Ports = append(c.Ports, n)
		}
	}
	switch c.DataMode {
	case "overlay", "tmpfs":
	default:
		return fmt.Errorf("ANDROID_DATA_MODE must be overlay or tmpfs, not %q", c.DataMode)
	}
	switch c.Net {
	case "veth", "isolated", "shared":
	default:
		return fmt.Errorf("ANDROID_NET must be veth, isolated or shared, not %q", c.Net)
	}
	if !strings.HasPrefix(c.Root, "/") {
		return fmt.Errorf("ANDROID_ROOT must be absolute: %q", c.Root)
	}
	return nil
}

// parseShellConf lee líneas KEY=VALUE de un fichero pensado para "source" de
// bash: comentarios, comillas simples o dobles alrededor del valor. No expande
// nada (build-image.sh no escribe expansiones).
func parseShellConf(r io.Reader) (map[string]string, error) {
	kv := map[string]string{}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("line %d: not KEY=VALUE", n)
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		kv[k] = v
	}
	return kv, sc.Err()
}
