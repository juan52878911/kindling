package main

// kling graph from-compose: traduce un compose.yaml a un fichero de grafo
// (docs/grafos.md#desde-un-composeyaml). Servicios → nodos; depends_on →
// aristas link (a cada puerto del destino) y depends (la sonda de listo);
// volúmenes con nombre → volúmenes de kling; healthcheck → depends con puerto.
//
// Un nodo de grafo no lleva entorno ni comando: viven en la imagen. Por eso la
// traducción tiene dos salidas: el grafo y las órdenes previas (kling volume
// create, kling image import -e … -- cmd…). Lo que no tiene equivalente se
// avisa, una línea por cosa, nunca se descarta en silencio.

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// traduccionCompose es el resultado de traducir un compose.
type traduccionCompose struct {
	Grafo   string   // el YAML del grafo
	Antes   []string // órdenes a correr antes de `graph up`
	Avisos  []string // una línea por cosa sin equivalente
	Proyect string
	// Publica son los puertos publicados en el host por nodo: {host, invitado}.
	// `graph up` no los publica; `kling compose up` sí, en 127.0.0.1.
	Publica map[string][][2]int

	// base es el directorio del compose: con él, un bind mount (./src:/app) es
	// una carpeta compartida rw del nodo. Vacío, se avisa y se salta.
	base string

	// vivos son los servicios traducidos, en orden, y por, todos por nombre
	// del compose: compose up los necesita para sus plantillas.
	vivos []*servicioCompose
	por   map[string]*servicioCompose
	// desde: nodo → plantilla de la que arranca en vez de su imagen
	// (compose up, ver compose_plantillas.go). Vacío en from-compose.
	desde map[string]string
	// saludTCP: el daemon no ejecuta healthchecks en las aristas depends
	// (sin api.CapabilityGraphHealth); service_healthy se aproxima con una
	// sonda TCP, como antes.
	saludTCP bool
}

// puertosDeImagen: los puertos de las imágenes de siempre, para un destino de
// depends_on que no declara ninguno (se avisa).
var puertosDeImagen = map[string]int{
	"postgres": 5432, "redis": 6379, "mysql": 3306, "mariadb": 3306,
	"mongo": 27017, "memcached": 11211, "rabbitmq": 5672,
}

var (
	reDNSNodo    = regexp.MustCompile(`[^a-z0-9-]+`)
	reVarCompose = regexp.MustCompile(`\$(\$|\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\})`)
)

type servicioCompose struct {
	nombre  string // el del compose
	nodo    string // el del grafo
	imagen  string
	puertos []int
	publ    [][2]int // {host, invitado} publicados en el host
	shares  []api.ShareSpec
	env     [][2]string
	cmd     []string
	entry   []string
	user    string
	restart string
	vcpus   int
	memMiB  int
	vols    []api.VolumeAttachment
	deps    []depCompose
	salud   []string // test del healthcheck, vacío si no hay
	// tiempos del healthcheck (interval, timeout, retries, start_period,
	// start_interval), como en una arista depends con health.
	saludT api.GraphHealth
	skip   bool
	// redes son las del compose (networks:); vacío es la "default". Dos
	// servicios se alcanzan si comparten una. sinRed: network_mode: none.
	redes  []string
	sinRed bool
	// refs son los servicios que su entorno nombra como host (db, @db:5432).
	refs map[string]bool
}

type depCompose struct {
	a    string // nombre en el compose
	cond string
}

// interpolarCompose resuelve ${VAR}, ${VAR:-def}, ${VAR-def} y $$ con lookup.
// Una variable sin valor ni defecto queda vacía y se avisa.
func interpolarCompose(src string, lookup func(string) (string, bool), aviso func(string)) string {
	return reVarCompose.ReplaceAllStringFunc(src, func(m string) string {
		if m == "$$" {
			return "$"
		}
		sub := reVarCompose.FindStringSubmatch(m)
		nombre, op, def := sub[2], sub[3], sub[4]
		v, ok := lookup(nombre)
		switch op {
		case ":-":
			if v == "" {
				return def
			}
		case "-":
			if !ok {
				return def
			}
		case ":?", "?":
			if v == "" {
				aviso(fmt.Sprintf("variable %s is required but not set", nombre))
			}
		case ":+", "+":
			if v != "" || (op == "+" && ok) {
				return def
			}
			return ""
		}
		if !ok {
			aviso(fmt.Sprintf("variable %s is not set; using an empty value", nombre))
		}
		return v
	})
}

// traducirCompose convierte el contenido de un compose.yaml. proyecto es el
// nombre del grafo si el fichero no lleva name: (o siempre, con forzar).
func traducirCompose(datos []byte, proyecto string, forzar bool, lookup func(string) (string, bool)) (*traduccionCompose, error) {
	return traducirComposeEn(datos, proyecto, forzar, lookup, "")
}

// traducirComposeEn es traducirCompose con el directorio del compose, del que
// cuelgan los bind mounts relativos (ver traduccionCompose.base).
func traducirComposeEn(datos []byte, proyecto string, forzar bool, lookup func(string) (string, bool), base string) (*traduccionCompose, error) {
	t := &traduccionCompose{base: base, Publica: map[string][][2]int{}}
	avisa := func(f string, a ...any) { t.Avisos = append(t.Avisos, "warning: "+fmt.Sprintf(f, a...)) }

	texto := interpolarCompose(string(datos), lookup, func(s string) { avisa("%s", s) })
	raiz, err := parseYAML([]byte(texto))
	if err != nil {
		return nil, err
	}
	top, ok := raiz.(map[string]any)
	if !ok {
		return nil, errors.New("a compose file is a map with a services: section")
	}
	if n, ok := top["name"].(string); ok && n != "" && !forzar {
		proyecto = n
	}
	proyecto = nombreDNS(proyecto)
	if err := api.ValidateGraphName(proyecto); err != nil {
		return nil, fmt.Errorf("graph name %q: %w (use -name)", proyecto, err)
	}
	t.Proyect = proyecto
	servs, ok := top["services"].(map[string]any)
	if !ok || len(servs) == 0 {
		return nil, errors.New("no services: section")
	}

	for _, k := range clavesOrd(top) {
		switch k {
		case "services", "name", "version":
		case "volumes", "networks":
		case "secrets", "configs":
			avisa("top-level %s: not supported; its values are not carried to the nodes", k)
		default:
			if !strings.HasPrefix(k, "x-") {
				avisa("top-level %s: not supported", k)
			}
		}
	}

	// Volúmenes con nombre declarados.
	declarados := map[string]string{} // compose → kling
	if vm, ok := top["volumes"].(map[string]any); ok {
		for _, n := range clavesOrd(vm) {
			kn := nombreVolumen(proyecto + "-" + n)
			declarados[n] = kn
			if cfg, ok := vm[n].(map[string]any); ok {
				for _, ck := range clavesOrd(cfg) {
					switch ck {
					case "name":
						avisa("volume %s: name: not supported (it is created as %s)", n, kn)
					default:
						avisa("volume %s: %s: not supported", n, ck)
					}
				}
			}
			t.Antes = append(t.Antes, "kling volume create "+kn)
		}
	}

	// Redes declaradas: solo dicen quién alcanza a quién (ver alcanza).
	redes := map[string]bool{"default": true}
	if nm, ok := top["networks"].(map[string]any); ok {
		for _, n := range clavesOrd(nm) {
			redes[n] = true
			if cfg, ok := nm[n].(map[string]any); ok {
				for _, ck := range clavesOrd(cfg) {
					avisa("network %s: %s: not supported (a network here only says which services reach each other)", n, ck)
				}
			}
		}
	}

	porNombre := map[string]*servicioCompose{}
	var orden []string
	usados := map[string]string{}
	for _, n := range clavesOrd(servs) {
		def, ok := servs[n].(map[string]any)
		if !ok {
			avisa("service %s: empty definition; skipped", n)
			continue
		}
		s := &servicioCompose{nombre: n, nodo: nombreDNS(n)}
		if s.nodo != n {
			avisa("service %s: renamed to node %q (a node is a DNS label: lowercase letters, digits and '-')", n, s.nodo)
		}
		if o, dup := usados[s.nodo]; dup {
			avisa("service %s: node name %q already used by %s; skipped", n, s.nodo, o)
			continue
		}
		usados[s.nodo] = n
		t.servicio(s, def, proyecto, declarados, avisa)
		for _, r := range s.redes {
			if !redes[r] {
				avisa("service %s: networks: %s is not declared at the top level; used as is", n, r)
			}
		}
		porNombre[n] = s
		orden = append(orden, n)
	}

	// depends_on hacia servicios saltados o inexistentes.
	for _, n := range orden {
		s := porNombre[n]
		var ok []depCompose
		for _, d := range s.deps {
			if o := porNombre[d.a]; o == nil || o.skip {
				avisa("service %s: depends_on %s: no such translated service; dropped", n, d.a)
				continue
			}
			ok = append(ok, d)
		}
		s.deps = ok
	}

	// Puertos inferidos para los servicios sin ninguno: como en la red de un
	// proyecto de Docker, cualquiera de los demás puede querer llegar a él.
	for _, n := range orden {
		o := porNombre[n]
		if o.skip || len(o.puertos) > 0 {
			continue
		}
		if p, ok := puertosDeImagen[imagenBase(o.imagen)]; ok {
			o.puertos = []int{p}
			avisa("service %s: no ports; assuming %d from image %s so the other services can reach it", o.nombre, p, o.imagen)
		}
	}

	// Referencias por nombre en el entorno: db → db.graph, para todo servicio
	// que se alcanza (comparte red), no solo los de depends_on.
	for _, n := range orden {
		s := porNombre[n]
		alcanzables := map[string]*servicioCompose{}
		for m, o := range porNombre {
			if m != n && alcanza(s, o) {
				alcanzables[m] = o
			}
		}
		for i, kv := range s.env {
			nv, cambios := reescribirHosts(kv[0], kv[1], alcanzables)
			for _, c := range cambios {
				if s.refs == nil {
					s.refs = map[string]bool{}
				}
				s.refs[c] = true
				avisa("service %s: environment %s: host %q rewritten to %q (a node is reached as <node>.%s)", n, kv[0], c, c+"."+api.GraphDomain, api.GraphDomain)
			}
			s.env[i][1] = nv
		}
	}

	var vivos []*servicioCompose
	for _, n := range orden {
		if s := porNombre[n]; !s.skip {
			vivos = append(vivos, s)
		}
	}
	if len(vivos) == 0 {
		return nil, errors.New("no service could be translated")
	}

	// Órdenes de import.
	for _, s := range vivos {
		t.Antes = append(t.Antes, ordenImport(s, proyecto))
		if len(s.publ) > 0 {
			t.Publica[s.nodo] = s.publ
		}
	}
	sort.SliceStable(t.Antes, func(i, j int) bool {
		return strings.HasPrefix(t.Antes[i], "kling volume") && !strings.HasPrefix(t.Antes[j], "kling volume")
	})

	t.vivos, t.por = vivos, porNombre
	t.Grafo = grafoDeCompose(t, proyecto, vivos, porNombre, avisa)
	// Se valida con el mismo camino que `graph up`: lo que sale se levanta.
	if _, err := leerGrafo(proyecto+".yaml", []byte(t.Grafo), nil); err != nil {
		return t, fmt.Errorf("the translated graph is not valid: %w", err)
	}
	return t, nil
}

// servicio rellena s con lo que entiende de def y avisa del resto.
func (t *traduccionCompose) servicio(s *servicioCompose, def map[string]any, proyecto string, vols map[string]string, avisa func(string, ...any)) {
	pre := "service " + s.nombre + ": "
	if img, ok := def["image"].(string); ok {
		s.imagen = img
	}
	if _, ok := def["build"]; ok {
		if s.imagen == "" {
			avisa("%sbuild: not supported and no image: to use; service skipped (build the image and import it with kling image import)", pre)
			s.skip = true
			return
		}
		avisa("%sbuild: not supported; using image %s as is", pre, s.imagen)
	}
	if s.imagen == "" {
		avisa("%sno image: or build:; service skipped", pre)
		s.skip = true
		return
	}
	for _, k := range clavesOrd(def) {
		v := def[k]
		switch k {
		case "image", "build":
		case "ports", "expose":
			t.puertosDe(s, k, v, pre, avisa)
		case "environment":
			t.entorno(s, v, pre, avisa)
		case "command":
			s.cmd = argvCompose(v)
		case "entrypoint":
			s.entry = argvCompose(v)
		case "user":
			s.user = fmt.Sprint(v)
		case "restart":
			switch fmt.Sprint(v) {
			case "always", "unless-stopped":
				s.restart = "always"
			case "on-failure":
				s.restart = "on-failure"
			case "no", "false":
				s.restart = "no"
			default:
				avisa("%srestart: %v: not supported", pre, v)
			}
		case "depends_on":
			t.dependencias(s, v, pre, avisa)
		case "volumes":
			t.volumes(s, v, vols, pre, avisa)
		case "healthcheck":
			t.healthcheck(s, v, pre, avisa)
		case "mem_limit", "mem_reservation":
			if k == "mem_reservation" {
				avisa("%s%s: not supported", pre, k)
				break
			}
			s.memMiB = memoriaMiB(fmt.Sprint(v), pre+k, avisa)
		case "cpus":
			if f, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil && f > 0 {
				s.vcpus = int(math.Ceil(f))
			} else {
				avisa("%scpus: %v: not a number", pre, v)
			}
		case "deploy":
			t.deploy(s, v, pre, avisa)
		case "networks":
			t.redesDe(s, v, pre, avisa)
		case "network_mode":
			switch m := fmt.Sprint(v); m {
			case "bridge", "default":
			case "none":
				s.sinRed = true
			default:
				avisa("%snetwork_mode: %s: %s", pre, m, motivoNoSoportado(k))
			}
		case "privileged", "cap_add", "cap_drop", "devices", "pid", "ipc",
			"security_opt", "sysctls", "ulimits", "tmpfs", "extra_hosts", "dns", "init",
			"working_dir", "hostname", "container_name", "env_file", "labels",
			"profiles", "links", "stdin_open", "tty", "stop_grace_period", "stop_signal",
			"logging", "shm_size", "platform", "pull_policy", "secrets", "configs", "domainname",
			"read_only", "group_add", "extends", "scale":
			avisa("%s%s: %s", pre, k, motivoNoSoportado(k))
		default:
			if !strings.HasPrefix(k, "x-") {
				avisa("%s%s: not supported", pre, k)
			}
		}
	}
}

func motivoNoSoportado(k string) string {
	switch k {
	case "privileged", "cap_add", "cap_drop", "devices", "security_opt", "pid", "ipc":
		return "not supported (a node is a microVM with its own kernel; there is no host privilege to grant)"
	case "network_mode", "links", "extra_hosts", "dns", "hostname", "domainname":
		return "not supported (a service reaches the others of its networks only by edges, as <node>.graph)"
	case "env_file":
		return "not read; put the variables in environment: or bake them with kling image import -env-file"
	case "labels":
		return "not carried (labels of a node are reserved for the daemon here)"
	case "container_name":
		return "ignored (the machine is called <graph>-<node>)"
	case "working_dir":
		return "not supported (use the image's WORKDIR)"
	case "profiles", "scale", "extends":
		return "not supported"
	case "tmpfs", "shm_size", "ulimits", "sysctls":
		return "not carried to the node (see kling image import -ulimit / -sysctl)"
	}
	return "not supported"
}

func (t *traduccionCompose) puertosDe(s *servicioCompose, k string, v any, pre string, avisa func(string, ...any)) {
	l, ok := v.([]any)
	if !ok {
		avisa("%s%s: expected a list", pre, k)
		return
	}
	for _, e := range l {
		var spec string
		switch x := e.(type) {
		case map[string]any:
			spec = fmt.Sprint(x["target"])
			if p, ok := x["published"]; ok && p != nil {
				spec = fmt.Sprintf("%v:%s", p, spec)
			}
		default:
			spec = fmt.Sprint(e)
		}
		spec = strings.TrimSuffix(spec, "/tcp")
		if strings.HasSuffix(spec, "/udp") {
			avisa("%s%s: %s: UDP is not supported", pre, k, spec)
			continue
		}
		parts := strings.Split(spec, ":")
		cont := parts[len(parts)-1]
		if strings.Contains(cont, "-") {
			avisa("%s%s: %s: port ranges are not supported", pre, k, spec)
			continue
		}
		p, err := strconv.Atoi(cont)
		if err != nil || p < 1 || p > 65535 {
			avisa("%s%s: %s: not a port", pre, k, spec)
			continue
		}
		if p == api.GuestPort {
			avisa("%s%s: %s: port %d is the kindling guest agent; no edge can reach it", pre, k, spec, p)
			continue
		}
		if k == "ports" && len(parts) > 1 {
			h, err := strconv.Atoi(parts[len(parts)-2])
			if err != nil || h < 1 || h > 65535 {
				avisa("%sports: %s: host port not understood", pre, spec)
			} else {
				s.publ = append(s.publ, [2]int{h, p})
				avisa("%sports: %s: graph up does not publish it on the host (kling compose up does, on 127.0.0.1:%d)", pre, spec, h)
			}
		}
		if !slicesHas(s.puertos, p) {
			s.puertos = append(s.puertos, p)
		}
	}
	sort.Ints(s.puertos)
}

func (t *traduccionCompose) entorno(s *servicioCompose, v any, pre string, avisa func(string, ...any)) {
	add := func(k, val string) { s.env = append(s.env, [2]string{k, val}) }
	switch x := v.(type) {
	case map[string]any:
		for _, k := range clavesOrd(x) {
			if x[k] == nil {
				avisa("%senvironment %s: no value (it would come from the host's environment); not carried", pre, k)
				continue
			}
			add(k, fmt.Sprint(x[k]))
		}
	case []any:
		for _, e := range x {
			kv := fmt.Sprint(e)
			k, val, ok := strings.Cut(kv, "=")
			if !ok {
				avisa("%senvironment %s: no value (it would come from the host's environment); not carried", pre, kv)
				continue
			}
			add(k, val)
		}
		sort.SliceStable(s.env, func(i, j int) bool { return s.env[i][0] < s.env[j][0] })
	default:
		avisa("%senvironment: expected a map or a list", pre)
	}
	if len(s.env) > 0 {
		avisa("%senvironment: a node has no environment of its own; the %d variable(s) are baked into the image with kling image import -e (every copy of it sees them)", pre, len(s.env))
	}
}

// redesDe lee el networks: de un servicio: una lista o un mapa (sus opciones,
// aliases o ipv4_address, no se llevan).
func (t *traduccionCompose) redesDe(s *servicioCompose, v any, pre string, avisa func(string, ...any)) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			s.redes = append(s.redes, fmt.Sprint(e))
		}
	case map[string]any:
		for _, n := range clavesOrd(x) {
			s.redes = append(s.redes, n)
			if m, ok := x[n].(map[string]any); ok {
				for _, mk := range clavesOrd(m) {
					avisa("%snetworks %s: %s: not supported", pre, n, mk)
				}
			}
		}
	default:
		avisa("%snetworks: expected a list or a map", pre)
	}
	sort.Strings(s.redes)
}

// alcanza dice si s llega a o por la red del proyecto: los dos tienen red y
// comparten una (sin networks:, la "default" de Docker Compose).
func alcanza(s, o *servicioCompose) bool {
	if s == o || s.sinRed || o.sinRed {
		return false
	}
	rs, ro := s.redes, o.redes
	if len(rs) == 0 {
		rs = []string{"default"}
	}
	if len(ro) == 0 {
		ro = []string{"default"}
	}
	for _, r := range rs {
		if slices.Contains(ro, r) {
			return true
		}
	}
	return false
}

func (t *traduccionCompose) dependencias(s *servicioCompose, v any, pre string, avisa func(string, ...any)) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			s.deps = append(s.deps, depCompose{a: fmt.Sprint(e), cond: "service_started"})
		}
	case map[string]any:
		for _, n := range clavesOrd(x) {
			c := "service_started"
			if m, ok := x[n].(map[string]any); ok {
				if cc, ok := m["condition"].(string); ok {
					c = cc
				}
				for _, mk := range clavesOrd(m) {
					if mk != "condition" {
						avisa("%sdepends_on %s: %s: not supported", pre, n, mk)
					}
				}
			}
			switch c {
			case "service_started", "service_healthy":
			case "service_completed_successfully":
				avisa("%sdepends_on %s: service_completed_successfully: not supported; treated as service_started", pre, n)
				c = "service_started"
			default:
				avisa("%sdepends_on %s: condition %q: not supported; treated as service_started", pre, n, c)
				c = "service_started"
			}
			s.deps = append(s.deps, depCompose{a: n, cond: c})
		}
	default:
		avisa("%sdepends_on: expected a list or a map", pre)
	}
	sort.SliceStable(s.deps, func(i, j int) bool { return s.deps[i].a < s.deps[j].a })
}

func (t *traduccionCompose) volumes(s *servicioCompose, v any, decl map[string]string, pre string, avisa func(string, ...any)) {
	l, ok := v.([]any)
	if !ok {
		avisa("%svolumes: expected a list", pre)
		return
	}
	for _, e := range l {
		var src, dst string
		var ro bool
		desc := fmt.Sprint(e)
		switch x := e.(type) {
		case map[string]any:
			typ := fmt.Sprint(x["type"])
			src, dst = fmt.Sprint(x["source"]), fmt.Sprint(x["target"])
			ro, _ = x["read_only"].(bool)
			desc = src + ":" + dst
			if typ != "volume" {
				avisa("%svolumes: %s: type %s is not supported (only named volumes)", pre, desc, typ)
				continue
			}
		default:
			p := strings.Split(desc, ":")
			switch len(p) {
			case 1:
				avisa("%svolumes: %s: an anonymous volume has no equivalent; declare a named one", pre, desc)
				continue
			case 3:
				ro = p[2] == "ro"
				if p[2] != "ro" && p[2] != "rw" {
					avisa("%svolumes: %s: option %q is not supported", pre, desc, p[2])
				}
				fallthrough
			case 2:
				src, dst = p[0], p[1]
			default:
				avisa("%svolumes: %s: not understood", pre, desc)
				continue
			}
		}
		if strings.ContainsAny(src[:min(1, len(src))], "./~") {
			if t.base == "" || strings.HasPrefix(src, "~") {
				avisa("%svolumes: %s: bind mounts are not supported here (kling compose up makes them live shares; or use a named volume)", pre, desc)
				continue
			}
			abs := src
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(t.base, src)
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				avisa("%svolumes: %s: only directories can be shared (a single file, or missing); skipped", pre, desc)
				continue
			}
			mode := "rw"
			if ro {
				mode = "ro"
			}
			s.shares = append(s.shares, api.ShareSpec{Source: filepath.Clean(abs), Mount: dst, Mode: mode})
			avisa("%svolumes: %s: live %s share of %s (it must be under daemon.share_roots)", pre, desc, mode, filepath.Clean(abs))
			continue
		}
		kn, ok := decl[src]
		if !ok {
			avisa("%svolumes: %s: %q is not declared under top-level volumes; skipped", pre, desc, src)
			continue
		}
		s.vols = append(s.vols, api.VolumeAttachment{Name: kn, Mount: dst, ReadOnly: ro})
		// Un volumen de kling es un ext4 recién hecho, con lost+found: initdb
		// no inicializa en la raíz de un punto de montaje.
		if imagenBase(s.imagen) == "postgres" && dst == "/var/lib/postgresql/data" && !ro {
			if !slices.ContainsFunc(s.env, func(kv [2]string) bool { return kv[0] == "PGDATA" }) {
				s.env = append(s.env, [2]string{"PGDATA", dst + "/pgdata"})
				avisa("%svolumes: %s: a new volume has a lost+found that initdb refuses; PGDATA=%s/pgdata is set (baked into the image)", pre, desc, dst)
			}
		}
	}
}

func (t *traduccionCompose) healthcheck(s *servicioCompose, v any, pre string, avisa func(string, ...any)) {
	m, ok := v.(map[string]any)
	if !ok {
		avisa("%shealthcheck: expected a map", pre)
		return
	}
	for _, k := range clavesOrd(m) {
		switch k {
		case "test":
			switch x := m[k].(type) {
			case string:
				s.salud = []string{x}
			case []any:
				for _, a := range x {
					s.salud = append(s.salud, fmt.Sprint(a))
				}
			}
			if len(s.salud) == 1 && s.salud[0] == "NONE" || len(s.salud) == 0 {
				s.salud = nil
			}
		case "disable":
			if b, _ := m[k].(bool); b {
				s.salud = nil
			}
		case "interval", "timeout", "start_period", "start_interval":
			d, err := time.ParseDuration(fmt.Sprint(m[k]))
			if err != nil || d < 0 {
				avisa("%shealthcheck %s: %v: not a duration (like 5s or 1m30s)", pre, k, m[k])
				continue
			}
			switch k {
			case "interval":
				s.saludT.IntervalMS = int(d.Milliseconds())
			case "timeout":
				s.saludT.TimeoutSeconds = max(int(math.Ceil(d.Seconds())), 1)
			case "start_period":
				s.saludT.StartPeriodSeconds = int(math.Ceil(d.Seconds()))
			case "start_interval":
				s.saludT.StartIntervalMS = int(d.Milliseconds())
			}
		case "retries":
			n, err := strconv.Atoi(fmt.Sprint(m[k]))
			if err != nil || n < 0 {
				avisa("%shealthcheck retries: %v: not a count", pre, m[k])
				continue
			}
			s.saludT.Retries = n
		default:
			avisa("%shealthcheck %s: not supported", pre, k)
		}
	}
}

func (t *traduccionCompose) deploy(s *servicioCompose, v any, pre string, avisa func(string, ...any)) {
	m, ok := v.(map[string]any)
	if !ok {
		avisa("%sdeploy: expected a map", pre)
		return
	}
	for _, k := range clavesOrd(m) {
		if k != "resources" {
			avisa("%sdeploy %s: not supported", pre, k)
			continue
		}
		r, _ := m[k].(map[string]any)
		for _, rk := range clavesOrd(r) {
			lim, _ := r[rk].(map[string]any)
			if rk != "limits" {
				avisa("%sdeploy resources %s: not supported", pre, rk)
				continue
			}
			for _, lk := range clavesOrd(lim) {
				switch lk {
				case "memory":
					s.memMiB = memoriaMiB(fmt.Sprint(lim[lk]), pre+"deploy memory", avisa)
				case "cpus":
					if f, err := strconv.ParseFloat(fmt.Sprint(lim[lk]), 64); err == nil && f > 0 {
						s.vcpus = int(math.Ceil(f))
					} else {
						avisa("%sdeploy cpus: %v: not a number", pre, lim[lk])
					}
				default:
					avisa("%sdeploy limits %s: not supported", pre, lk)
				}
			}
		}
	}
}

// ordenImport es el `kling image import` del servicio: lleva su entorno,
// usuario, entrypoint, comando y política de reinicio.
func ordenImport(s *servicioCompose, proyecto string) string {
	return ordenImportCon(s, proyecto, true, true, true)
}

// ordenImportCon es la orden de import del servicio; sin entorno ni comando
// cuando los lleva la máquina (compose up con plantillas), y sin -replace
// cuando se confía en que import reconozca la misma receta.
func ordenImportCon(s *servicioCompose, proyecto string, conEntorno, conComando, replace bool) string {
	a := []string{"kling", "image", "import", comillaSh(s.imagen), "-name", proyecto + "-" + s.nodo}
	if replace {
		a = append(a, "-replace")
	}
	if s.user != "" {
		a = append(a, "-user", comillaSh(s.user))
	}
	if s.restart != "" {
		a = append(a, "-restart", s.restart)
	}
	for _, e := range s.entry {
		if !conComando {
			break
		}
		a = append(a, "-entrypoint", comillaSh(e))
	}
	for _, kv := range s.env {
		if !conEntorno {
			break
		}
		a = append(a, "-e", comillaSh(kv[0]+"="+kv[1]))
	}
	if len(s.cmd) > 0 && conComando {
		a = append(a, "--")
		for _, c := range s.cmd {
			a = append(a, comillaSh(c))
		}
	}
	return strings.Join(a, " ")
}

// grafoDeCompose emite el YAML del grafo, con las órdenes previas en la
// cabecera como comentario.
func grafoDeCompose(t *traduccionCompose, proyecto string, vivos []*servicioCompose, por map[string]*servicioCompose, avisa func(string, ...any)) string {
	var b strings.Builder
	b.WriteString("# Generated by kling graph from-compose. Run these first:\n")
	for _, l := range t.Antes {
		b.WriteString("#   " + l + "\n")
	}
	b.WriteString("name: " + yq(proyecto) + "\nnodes:\n")
	for _, s := range vivos {
		campos := []string{"image: " + yq(proyecto+"-"+s.nodo)}
		if p := t.desde[s.nodo]; p != "" {
			// Las vCPU y la memoria son las de la plantilla.
			campos = []string{"from: " + yq(p)}
		} else {
			if s.vcpus > 0 {
				campos = append(campos, "vcpus: "+strconv.Itoa(s.vcpus))
			}
			if s.memMiB > 0 {
				campos = append(campos, "mem_mib: "+strconv.Itoa(s.memMiB))
			}
			if exigeExec(s, vivos) && !t.saludTCP {
				// Su healthcheck se ejecuta dentro (arista depends con health).
				campos = append(campos, "allow_exec: true")
			}
		}
		if len(s.puertos) > 0 {
			ps := make([]string, len(s.puertos))
			for i, p := range s.puertos {
				ps[i] = strconv.Itoa(p)
			}
			campos = append(campos, "ports: ["+strings.Join(ps, ", ")+"]")
		}
		if len(s.vols) > 0 {
			vs := make([]string, len(s.vols))
			for i, v := range s.vols {
				vs[i] = "{name: " + yq(v.Name) + ", mount: " + yq(v.Mount)
				if v.ReadOnly {
					vs[i] += ", read_only: true"
				}
				vs[i] += "}"
			}
			campos = append(campos, "volumes: ["+strings.Join(vs, ", ")+"]")
		}
		if len(s.shares) > 0 {
			ss := make([]string, len(s.shares))
			for i, sh := range s.shares {
				ss[i] = "{source: " + yq(sh.Source) + ", mount: " + yq(sh.Mount) + ", mode: " + sh.Mode + "}"
			}
			campos = append(campos, "shares: ["+strings.Join(ss, ", ")+"]")
		}
		fmt.Fprintf(&b, "  %s: {%s}\n", s.nodo, strings.Join(campos, ", "))
	}

	// Enlaces: como en la red de un proyecto de Docker Compose, cada servicio
	// alcanza a todos los de sus redes, no solo a los de depends_on. Si no
	// caben en el tope de aristas, solo los que se usan: depends_on y los
	// hosts que nombra el entorno.
	enlaces := enlacesCompose(vivos, por, false, avisa)
	var deps []string
	for _, s := range vivos {
		for _, d := range s.deps {
			o := por[d.a]
			if len(o.puertos) == 0 {
				avisa("service %s: depends_on %s: %s exposes no ports, so there is only an ordering edge (no link); add ports: to it", s.nombre, d.a, d.a)
			} else if !alcanza(s, o) {
				avisa("service %s: depends_on %s: they share no network; only an ordering edge", s.nombre, d.a)
			}
			dep := fmt.Sprintf("  - {from: %s, to: %s, kind: depends", s.nodo, o.nodo)
			if d.cond == "service_healthy" {
				puerto := 0
				for _, p := range o.puertos {
					if !puertoReservado(p) {
						puerto = p
						break
					}
				}
				switch {
				case len(o.salud) == 0:
					avisa("service %s: depends_on %s: service_healthy but %s has no healthcheck; waiting for it to be running only", s.nombre, d.a, d.a)
				case len(saludArgv(o.salud)) > 0 && !t.saludTCP:
					// El test de verdad, dentro del destino (api.GraphEdge.Health).
					h := o.saludT
					h.Test = saludArgv(o.salud)
					dep += ", health: " + saludYAML(h)
				case puerto == 0:
					avisa("service %s: depends_on %s: service_healthy with no port to probe; waiting for it to be running only", s.nombre, d.a)
				default:
					p := puertoDeSalud(o.salud, o.puertos, puerto)
					dep += fmt.Sprintf(", port: %d", p)
					avisa("service %s: depends_on %s: healthcheck %q approximated by a TCP probe on port %d", s.nombre, d.a, strings.Join(o.salud, " "), p)
				}
			}
			deps = append(deps, dep+"}")
		}
	}
	if len(enlaces)+len(deps) > api.GraphMaxEdges {
		avisa("the project network needs %d edges, over the limit of %d: only the links to depends_on and to the hosts named in environment are kept",
			len(enlaces)+len(deps), api.GraphMaxEdges)
		enlaces = enlacesCompose(vivos, por, true, func(string, ...any) {})
	}
	aristas := append(enlaces, deps...)
	sort.SliceStable(aristas, func(i, j int) bool { return claveArista(aristas[i]) < claveArista(aristas[j]) })
	for _, s := range vivos {
		if len(s.salud) == 0 {
			continue
		}
		usado := false
		for _, x := range vivos {
			for _, d := range x.deps {
				if d.a == s.nombre && d.cond == "service_healthy" {
					usado = true
				}
			}
		}
		if !usado {
			avisa("service %s: healthcheck: nobody depends on it with service_healthy; not carried", s.nombre)
		}
	}
	if len(aristas) > 0 {
		b.WriteString("edges:\n" + strings.Join(aristas, "\n") + "\n")
	}
	return b.String()
}

// saludYAML es h como mapa de flujo del fichero del grafo.
func saludYAML(h api.GraphHealth) string {
	ts := make([]string, len(h.Test))
	for i, a := range h.Test {
		ts[i] = yq(a)
	}
	campos := []string{"test: [" + strings.Join(ts, ", ") + "]"}
	for _, c := range []struct {
		k string
		v int
	}{{"interval_ms", h.IntervalMS}, {"timeout_seconds", h.TimeoutSeconds}, {"retries", h.Retries},
		{"start_period_seconds", h.StartPeriodSeconds}, {"start_interval_ms", h.StartIntervalMS}} {
		if c.v > 0 {
			campos = append(campos, c.k+": "+strconv.Itoa(c.v))
		}
	}
	return "{" + strings.Join(campos, ", ") + "}"
}

// exigeExec dice si algún depends_on service_healthy ejecuta el test dentro
// de s: entonces su nodo necesita allow_exec.
func exigeExec(s *servicioCompose, vivos []*servicioCompose) bool {
	if len(saludArgv(s.salud)) == 0 {
		return false
	}
	for _, x := range vivos {
		for _, d := range x.deps {
			if d.a == s.nombre && d.cond == "service_healthy" {
				return true
			}
		}
	}
	return false
}

// puertoReservado: los puertos que no valen en una arista link (el DNS y el
// proxy de credenciales de la dirección del grafo).
func puertoReservado(p int) bool { return p == 53 || p == 80 || p == 443 }

// enlacesCompose son las aristas link de la red del proyecto: de cada
// servicio a cada puerto de cada servicio que comparte red con él. Un nodo
// alcanza un solo nodo por puerto: si dos destinos exponen el mismo, gana el
// de depends_on o el que nombra el entorno, y luego el primero por nombre.
// soloUsados deja solo esos (depends_on y entorno).
func enlacesCompose(vivos []*servicioCompose, por map[string]*servicioCompose, soloUsados bool, avisa func(string, ...any)) []string {
	var out []string
	for _, s := range vivos {
		usado := func(o *servicioCompose) bool {
			if s.refs[o.nombre] {
				return true
			}
			for _, d := range s.deps {
				if d.a == o.nombre {
					return true
				}
			}
			return false
		}
		var destinos []*servicioCompose
		for _, o := range vivos {
			if alcanza(s, o) && (!soloUsados || usado(o)) {
				destinos = append(destinos, o)
			}
		}
		sort.SliceStable(destinos, func(i, j int) bool { return usado(destinos[i]) && !usado(destinos[j]) })
		quien := map[int]*servicioCompose{}
		for _, o := range destinos {
			for _, p := range o.puertos {
				if puertoReservado(p) {
					if usado(o) {
						avisa("service %s: reaching %s: port %d is reserved on the graph address; no link edge to it (expose the service on another port)", s.nombre, o.nombre, p)
					}
					continue
				}
				if prev := quien[p]; prev != nil {
					if usado(o) {
						avisa("service %s: reaching %s: port %d already reaches %s (a node reaches one node per port); no link edge to it", s.nombre, o.nombre, p, prev.nombre)
					}
					continue
				}
				quien[p] = o
				out = append(out, fmt.Sprintf("  - {from: %s, to: %s, kind: link, port: %d}", s.nodo, o.nodo, p))
			}
		}
	}
	return out
}

// claveArista ordena las aristas por origen y destino, y dentro de cada par
// los link antes que la depends: el orden de siempre.
func claveArista(l string) string {
	f := strings.Fields(strings.NewReplacer("{", " ", "}", " ", ",", " ").Replace(l))
	// - from: X to: Y kind: K ...
	k := ""
	if len(f) > 6 {
		orden := "1"
		if f[6] == "link" {
			orden = "0"
		}
		k = f[2] + "\x00" + f[4] + "\x00" + orden
	}
	return k
}

// puertoDeSalud: el puerto del destino que nombra el test del healthcheck, o
// el primero.
func puertoDeSalud(test []string, puertos []int, def int) int {
	txt := strings.Join(test, " ")
	for _, p := range puertos {
		if regexp.MustCompile(`(^|\D)` + strconv.Itoa(p) + `(\D|$)`).MatchString(txt) {
			return p
		}
	}
	return def
}

// reHostVar: una variable cuyo valor entero puede ser un host (DB_HOST,
// REDIS_HOSTNAME, SMTP_SERVER, API_ADDR). Un valor suelto igual al nombre de
// un servicio solo se reescribe en ellas: POSTGRES_USER=app con un servicio
// app es un usuario, no un host.
var reHostVar = regexp.MustCompile(`(?i)(HOST|HOSTNAME|HOSTS|SERVER|SERVERS|ADDR|ADDRESS)$`)

// reescribirHosts cambia el nombre de un servicio usado como host en el valor
// de la variable k ("postgres://u:p@db:5432/x", "http://api:8081" en
// cualquiera; "db" suelto solo en una variable de host) por <nodo>.graph.
func reescribirHosts(k, v string, por map[string]*servicioCompose) (string, []string) {
	var cambios []string
	for _, n := range clavesOrd(por) {
		s := por[n]
		if s.skip {
			continue
		}
		if v == n && reHostVar.MatchString(k) {
			v = s.nodo + "." + api.GraphDomain
			cambios = append(cambios, n)
			continue
		}
		re := regexp.MustCompile(`(@|//)` + regexp.QuoteMeta(n) + `([:/?]|$)`)
		if re.MatchString(v) {
			v = re.ReplaceAllString(v, "${1}"+s.nodo+"."+api.GraphDomain+"${2}")
			cambios = append(cambios, n)
		}
	}
	return v, cambios
}

func nombreDNS(s string) string {
	s = reDNSNodo.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 24 {
		s = strings.Trim(s[:24], "-")
	}
	return s
}

func nombreVolumen(s string) string {
	s = strings.ToLower(s)
	return regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(s, "-")
}

func imagenBase(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		ref = ref[:i]
	}
	return ref
}

// memoriaMiB lee 512m, 1g, 256mb, 1073741824.
func memoriaMiB(s, donde string, avisa func(string, ...any)) int {
	m := regexp.MustCompile(`^(?i)(\d+(?:\.\d+)?)\s*([kmg]?)b?$`).FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		avisa("%s: %q is not a size", donde, s)
		return 0
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	switch strings.ToLower(m[2]) {
	case "k":
		f /= 1024
	case "":
		f /= 1 << 20
	case "g":
		f *= 1024
	}
	return int(math.Ceil(f))
}

// argvCompose: command/entrypoint como lista o como cadena (se parte como un
// shell sencillo: espacios y comillas).
func argvCompose(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, len(x))
		for i, a := range x {
			out[i] = fmt.Sprint(a)
		}
		return out
	case string:
		var out []string
		var cur strings.Builder
		var q rune
		has := false
		for _, r := range x {
			switch {
			case q != 0:
				if r == q {
					q = 0
				} else {
					cur.WriteRune(r)
				}
			case r == '"' || r == '\'':
				q, has = r, true
			case r == ' ' || r == '\t':
				if has || cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
					has = false
				}
			default:
				cur.WriteRune(r)
			}
		}
		if has || cur.Len() > 0 {
			out = append(out, cur.String())
		}
		return out
	}
	return nil
}

var reSh = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func comillaSh(s string) string {
	if reSh.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// yq escribe un escalar YAML: plano si es seguro, entre comillas si no.
func yq(s string) string {
	if s != "" && regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(s) {
		if _, esTexto := escalarPlano(s).(string); esTexto {
			return s
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func clavesOrd[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func slicesHas(l []int, x int) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// graphFromCompose es `kling graph from-compose`.
func graphFromCompose(args []string) error {
	fs := flag.NewFlagSet("graph from-compose", flag.ExitOnError)
	out := fs.String("o", "", "write the graph here (default: stdout)")
	name := fs.String("name", "", "graph name (default: name: of the file, or the directory's)")
	pre := fs.String("script", "", "also write the commands to run before graph up (volume create, image import) as a shell script")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return errors.New("usage: kling graph from-compose <compose.yaml> [-o graph.yaml] [-name N] [-script setup.sh]")
	}
	path := fs.Arg(0)
	datos, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	proyecto := *name
	if proyecto == "" {
		abs, _ := filepath.Abs(path)
		proyecto = filepath.Base(filepath.Dir(abs))
	}
	t, err := traducirCompose(datos, proyecto, *name != "", os.LookupEnv)
	if t != nil {
		for _, a := range t.Avisos {
			fmt.Fprintln(os.Stderr, a)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if *pre != "" {
		sh := "#!/bin/sh\nset -e\n" + strings.Join(t.Antes, "\n") + "\n"
		if err := os.WriteFile(*pre, []byte(sh), 0o755); err != nil {
			return err
		}
	}
	if *out == "" {
		_, err = fmt.Print(t.Grafo)
		return err
	}
	if err := os.WriteFile(*out, []byte(t.Grafo), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d warnings); run its header commands, then: kling graph up %s\n", *out, len(t.Avisos), *out)
	return nil
}
