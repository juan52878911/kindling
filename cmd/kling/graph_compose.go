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

	"github.com/juan52878911/kindling/pkg/api"
)

// traduccionCompose es el resultado de traducir un compose.
type traduccionCompose struct {
	Grafo   string   // el YAML del grafo
	Antes   []string // órdenes a correr antes de `graph up`
	Avisos  []string // una línea por cosa sin equivalente
	Proyect string
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
	nombre   string // el del compose
	nodo     string // el del grafo
	imagen   string
	puertos  []int
	hostPubl []string // "8080:80" publicados en el host
	env      [][2]string
	cmd      []string
	entry    []string
	user     string
	restart  string
	vcpus    int
	memMiB   int
	vols     []api.VolumeAttachment
	deps     []depCompose
	salud    []string // test del healthcheck, vacío si no hay
	skip     bool
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
	t := &traduccionCompose{}
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
		case "volumes", "networks", "secrets", "configs":
			if k != "volumes" {
				avisa("top-level %s: not supported (nodes of a graph share one network; reach each other only by edges)", k)
				if k != "networks" {
					avisa("top-level %s: its values are not carried to the nodes", k)
				}
			}
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

	// Puertos inferidos para destinos sin ninguno.
	for _, n := range orden {
		for _, d := range porNombre[n].deps {
			o := porNombre[d.a]
			if len(o.puertos) == 0 {
				if p, ok := puertosDeImagen[imagenBase(o.imagen)]; ok {
					o.puertos = []int{p}
					avisa("service %s: no ports; assuming %d from image %s so %s can reach it", o.nombre, p, o.imagen, n)
				}
			}
		}
	}

	// Referencias por nombre en el entorno: db → db.graph.
	for _, n := range orden {
		s := porNombre[n]
		for i, kv := range s.env {
			nv, cambios := reescribirHosts(kv[1], porNombre)
			for _, c := range cambios {
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
	}
	sort.SliceStable(t.Antes, func(i, j int) bool {
		return strings.HasPrefix(t.Antes[i], "kling volume") && !strings.HasPrefix(t.Antes[j], "kling volume")
	})

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
		case "privileged", "network_mode", "cap_add", "cap_drop", "devices", "pid", "ipc",
			"security_opt", "sysctls", "ulimits", "tmpfs", "extra_hosts", "dns", "init",
			"working_dir", "hostname", "container_name", "env_file", "labels", "networks",
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
	case "network_mode", "networks", "links", "extra_hosts", "dns", "hostname", "domainname":
		return "not supported (nodes share no network; they reach each other only by edges, as <node>.graph)"
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
			avisa("%sports: %s: publishing to the host is not supported; port %d is exposed to the graph's edges only", pre, spec, p)
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
			avisa("%svolumes: %s: bind mounts are not supported (use a named volume, or a share on a cold-booted image)", pre, desc)
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
		case "interval", "timeout", "retries", "start_period", "start_interval":
			avisa("%shealthcheck %s: ignored (the probe is a TCP connect, retried until the 2 minute limit)", pre, k)
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
	a := []string{"kling", "image", "import", comillaSh(s.imagen), "-name", proyecto + "-" + s.nodo, "-replace"}
	if s.user != "" {
		a = append(a, "-user", comillaSh(s.user))
	}
	if s.restart != "" {
		a = append(a, "-restart", s.restart)
	}
	for _, e := range s.entry {
		a = append(a, "-entrypoint", comillaSh(e))
	}
	for _, kv := range s.env {
		a = append(a, "-e", comillaSh(kv[0]+"="+kv[1]))
	}
	if len(s.cmd) > 0 {
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
		if s.vcpus > 0 {
			campos = append(campos, "vcpus: "+strconv.Itoa(s.vcpus))
		}
		if s.memMiB > 0 {
			campos = append(campos, "mem_mib: "+strconv.Itoa(s.memMiB))
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
		fmt.Fprintf(&b, "  %s: {%s}\n", s.nodo, strings.Join(campos, ", "))
	}

	var aristas []string
	for _, s := range vivos {
		for _, d := range s.deps {
			o := por[d.a]
			puerto := 0
			for _, p := range o.puertos {
				if p == 53 || p == 80 || p == 443 {
					avisa("service %s: depends_on %s: port %d is reserved on the graph address; no link edge to it (expose the service on another port)", s.nombre, d.a, p)
					continue
				}
				if puerto == 0 {
					puerto = p
				}
				aristas = append(aristas, fmt.Sprintf("  - {from: %s, to: %s, kind: link, port: %d}", s.nodo, o.nodo, p))
			}
			if len(o.puertos) == 0 {
				avisa("service %s: depends_on %s: %s exposes no ports, so there is only an ordering edge (no link); add ports: to it", s.nombre, d.a, d.a)
			}
			dep := fmt.Sprintf("  - {from: %s, to: %s, kind: depends", s.nodo, o.nodo)
			if d.cond == "service_healthy" {
				if len(o.salud) == 0 {
					avisa("service %s: depends_on %s: service_healthy but %s has no healthcheck; waiting for it to be running only", s.nombre, d.a, d.a)
				} else if puerto == 0 {
					avisa("service %s: depends_on %s: service_healthy with no port to probe; waiting for it to be running only", s.nombre, d.a)
				} else {
					p := puertoDeSalud(o.salud, o.puertos, puerto)
					dep += fmt.Sprintf(", port: %d", p)
					avisa("service %s: depends_on %s: healthcheck %q approximated by a TCP probe on port %d (the test itself is not run)", s.nombre, d.a, strings.Join(o.salud, " "), p)
				}
			}
			aristas = append(aristas, dep+"}")
		}
	}
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

// reescribirHosts cambia el nombre de un servicio usado como host en un valor
// ("db", "postgres://u:p@db:5432/x", "http://api:8081") por <nodo>.graph.
func reescribirHosts(v string, por map[string]*servicioCompose) (string, []string) {
	var cambios []string
	for _, n := range clavesOrd(por) {
		s := por[n]
		if s.skip {
			continue
		}
		if v == n {
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
