package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// El banco de IA de la ronda 8 (Z6): la fase `ia` de bench/ec2.sh, que lanza
// instancias spot con tope de gasto, y 96-von-bench.sh con salida JSON. Nada
// de esto toca AWS ni microVMs: un `aws` falso en el PATH apunta si alguien lo
// llama (en seco no debe llamarlo nadie) y un `kling` falso contesta al banco.

// awsFalso deja en dir un `aws` que apunta cada llamada en dir/aws.log y
// devuelve la ruta del log.
func awsFalso(t *testing.T, dir string) string {
	t.Helper()
	log := filepath.Join(dir, "aws.log")
	guion := "#!/bin/sh\necho \"$*\" >> '" + log + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(guion), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

// ec2 corre bench/ec2.sh con el aws falso delante en el PATH, sin terminal
// (stdin vacío) y con el libro de gasto en dir.
func ec2(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"bench/ec2.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GASTO_LIBRO="+filepath.Join(dir, "gasto.tsv"),
		"SALIDA="+filepath.Join(dir, "out"))
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sinLlamadas(t *testing.T, log string) {
	t.Helper()
	if b, err := os.ReadFile(log); err == nil {
		t.Fatalf("se llamó a aws de verdad:\n%s", b)
	}
}

func TestFaseIAEnDryRun(t *testing.T) {
	dir := t.TempDir()
	log := awsFalso(t, dir)
	out, err := ec2(t, dir, nil, "--dry-run", "ia")
	if err != nil {
		t.Fatalf("--dry-run ia: %v\n%s", err, out)
	}
	sinLlamadas(t, log)
	for _, want := range []string{
		"+ aws",           // imprime las llamadas en vez de hacerlas
		"run-instances",   // la de lanzar
		"c8i.4xlarge",     // tipo por defecto de la fase ia
		"MarketType=spot", // spot
		"MaxPrice=",       // con precio máximo
		"--instance-initiated-shutdown-behavior terminate",
		"shutdown -h +",  // apagado en el propio arranque
		"VolumeType=gp3", // disco gp3 que se borra al terminar
		"DeleteOnTermination=true",
		"ResourceType=volume", // también se etiqueta el disco
		"kbench-fase",         // etiqueta de la fase
		"USD",                 // coste estimado
		"terminate-instances", // y cómo se borra
		"remoto/ia.sh",        // lo que corre dentro
	} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en el plan en seco:\n%s", want, out)
		}
	}
	if strings.Contains(out, "remoto/setup.sh") {
		t.Errorf("la fase ia no instala k3s ni OpenSandbox (setup.sh):\n%s", out)
	}
}

// La cuenta no tiene cupo para un .metal (16 vCPU bajo demanda, < 48 spot):
// ec2.sh se niega antes de llamar a aws y dice por qué. Fuera de las
// familias 8i tampoco se pide NestedVirtualization (AWS la rechaza).
func TestFaseIAMetalSinAnidada(t *testing.T) {
	t.Run("metal se niega", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_TYPE=c7i.metal-24xl", "TOPE_MIN=120", "EC2_MAX_VCPU=192"}, "--dry-run", "ia")
		if err == nil || !strings.Contains(out, "se niega") || !strings.Contains(out, ".metal") {
			t.Fatalf("un .metal debía negarse con un mensaje claro: err=%v\n%s", err, out)
		}
		sinLlamadas(t, log)
	})
	t.Run("fuera de 8i sin anidada", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_TYPE=c7i.4xlarge", "EC2_PRECIO_USD_H=0.3"}, "--dry-run", "ia")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		sinLlamadas(t, log)
		if strings.Contains(out, "NestedVirtualization") {
			t.Errorf("c7i no lleva NestedVirtualization:\n%s", out)
		}
	})
	t.Run("más vCPU que el cupo se niega", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_TYPE=c8i.8xlarge", "EC2_PRECIO_USD_H=0.4"}, "--dry-run", "ia")
		if err == nil || !strings.Contains(out, "EC2_MAX_VCPU") {
			t.Fatalf("32 vCPU con cupo 16 debía negarse: err=%v\n%s", err, out)
		}
		sinLlamadas(t, log)
	})
}

// El tope de gasto se comprueba antes de nada, también en seco: el peor caso
// (precio máximo de spot × tope de minutos) más lo ya apuntado en el libro no
// puede pasar de PRESUPUESTO_USD (10).
func TestFaseIARespetaElPresupuesto(t *testing.T) {
	t.Run("una sola instancia demasiado cara", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_PRECIO_USD_H=1", "TOPE_MIN=720"}, "--dry-run", "ia")
		if err == nil || !strings.Contains(out, "presupuesto") {
			t.Fatalf("debía negarse por presupuesto: err=%v\n%s", err, out)
		}
		sinLlamadas(t, log)
	})
	t.Run("lo ya gastado cuenta", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		libro := "2026-11-23T10:00:00Z\tkbench-x\tc7i.metal-24xl\t9.80\n"
		if err := os.WriteFile(filepath.Join(dir, "gasto.tsv"), []byte(libro), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := ec2(t, dir, nil, "--dry-run", "ia")
		if err == nil || !strings.Contains(out, "presupuesto") {
			t.Fatalf("con 9,80 USD apuntados debía negarse: err=%v\n%s", err, out)
		}
		sinLlamadas(t, log)
	})
	t.Run("tipo sin precio conocido", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_TYPE=x9z.4xlarge"}, "--dry-run", "ia")
		if err == nil || !strings.Contains(out, "EC2_PRECIO_USD_H") {
			t.Fatalf("sin precio debía pedir EC2_PRECIO_USD_H: err=%v\n%s", err, out)
		}
		sinLlamadas(t, log)
	})
}

// Sin --si y sin terminal, lanzar de verdad se niega ANTES de la primera
// llamada a aws (ni la AMI se busca).
func TestFaseIAPideConfirmacion(t *testing.T) {
	dir := t.TempDir()
	log := awsFalso(t, dir)
	bins := filepath.Join(dir, "bins")
	if err := os.MkdirAll(bins, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"kling", "ia-bench"} {
		if err := os.WriteFile(filepath.Join(bins, f), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out, err := ec2(t, dir, []string{"BINS_DIR=" + bins}, "ia")
	if err == nil || !strings.Contains(out, "--si") {
		t.Fatalf("sin confirmación debía negarse y nombrar --si: err=%v\n%s", err, out)
	}
	sinLlamadas(t, log)
	if _, err := os.Stat(filepath.Join(dir, "gasto.tsv")); err == nil {
		t.Error("sin lanzar no se apunta gasto")
	}
}

// --limpiar-todo busca por la clave de etiqueta, no por un ID concreto.
func TestLimpiarTodoEnSeco(t *testing.T) {
	dir := t.TempDir()
	log := awsFalso(t, dir)
	out, err := ec2(t, dir, nil, "--limpiar-todo", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sinLlamadas(t, log)
	for _, want := range []string{"describe-instances", "tag-key", "kbench", "describe-volumes", "describe-security-groups"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q:\n%s", want, out)
		}
	}
}

// Los guiones del banco de IA pasan bash -n con el bash del sistema (3.2 en
// macOS) y shellcheck si está.
func TestGuionesIASintaxis(t *testing.T) {
	fs := []string{"96-von-bench.sh", "bench/ec2.sh", "bench/remoto/fases.sh", "bench/remoto/ia.sh", "bench/remoto/chispa-p3.sh"}
	for _, f := range fs {
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", f, err, out)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("sin shellcheck")
	}
	args := append([]string{"-x", "-S", "warning"}, fs...)
	if out, err := exec.Command("shellcheck", args...).CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// klingFalsoBanco contesta lo que 96-von-bench.sh pregunta, con el estado de las
// máquinas en ficheros de dir/m: una por fichero, con su estado dentro.
const klingFalsoBanco = `#!/usr/bin/env bash
D="$KFALSO"; mkdir -p "$D/m"
maquina() { printf '{"id":"%s0000000000","name":"%s","state":"%s","pid":%d,"thaw_ms":7,"freeze_ms":9,"disk_bytes":1048576}\n' "$1" "$1" "$(cat "$D/m/$1")" "$$"; }
nombre() { while [ $# -gt 0 ]; do [ "$1" = -name ] && { echo "$2"; return; }; shift; done; }
case "$1 $2" in
"snapshots -json") echo '[{"name":"von-falso","image":"img","vcpus":2,"mem_mib":512,"mem_bytes":536870912,"disk_bytes":600000000,"labels":{"von.model":"falso:q8_0","kling.ports":"8000"}}]' ;;
"status -v") echo '{"backend":"falso"}' ;;
"image ls") echo '[{"name":"img","disk_bytes":1048576}]' ;;
"ps -a") printf '['; s=""; for f in "$D"/m/*; do [ -e "$f" ] || continue; printf '%s' "$s"; maquina "$(basename "$f")"; s=,; done; echo ']' ;;
"top -json") # las máquinas que casan con KFALSO_DISCO (patrón de case) leyeron 4096 bytes de disco; el resto, 0
  printf '{"available_mib":1000,"machines":['; s=""; for f in "$D"/m/*; do [ -e "$f" ] || continue; [ "$(cat "$f")" = running ] || continue
    n=$(basename "$f"); dr=0; [ -n "${KFALSO_DISCO:-}" ] && case $n in $KFALSO_DISCO) dr=4096 ;; esac
    printf '%s{"name":"%s","pid":%d,"pss_mib":10,"disk_read_bytes":%d,"state":"running"}' "$s" "$n" "$$" "$dr"; s=,; done; echo ']}' ;;
"ai serve") shift 2; exec python3 -I "$KFALSO_GW" "$@" ;;
"ai model") echo '{"request_ms":3,"response":{"choices":[{"message":{"content":"x"}}],"timings":{"prompt_per_second":100,"predicted_per_second":10,"prompt_n":5,"predicted_n":8}}}' ;;
*)
  case "$1" in
  run) n=$(nombre "$@"); echo running > "$D/m/$n"; maquina "$n" ;;
  freeze) echo frozen > "$D/m/${!#}"; maquina "${!#}" ;;
  thaw) echo running > "$D/m/${!#}"; maquina "${!#}" ;;
  rm) for a in "$@"; do case $a in rm|-f) ;; *) rm -f "$D/m/$a" ;; esac; done ;;
  *) echo "kling falso: no sé $*" >&2; exit 1 ;;
  esac ;;
esac
`

// corridaFalsa deja en res lo que remoto/ia.sh guarda junto a los JSON y que
// ia.py exige desde la ronda 9 (C9-12): corrida.json con el journal del
// daemon y, por dorado fabricado, el desglose del commit y su service.log.
func corridaFalsa(t *testing.T, res, dorado string) {
	t.Helper()
	ficheros := map[string]string{
		"journal-kt-bench.log":              "nov 23 10:00:00 kling[1]: commit " + dorado + "-golden -> " + dorado + ": overlay 80 ms (total 900 ms)\n",
		"dorado-" + dorado + "/service.log": "listo\n",
		"corrida.json": `{"esquema":"ia-corrida/1","journal":{"unidad":"kt-bench","fichero":"journal-kt-bench.log"},` +
			`"dorados":[{"dorado":"` + dorado + `","rc":0,"fabricar_ms":40000,"commit":{"overlay":80,"total":900},` +
			`"service_log":"dorado-` + dorado + `/service.log"}]}`,
	}
	for f, c := range ficheros {
		p := filepath.Join(res, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 96-von-bench.sh saca un JSON con todas las claves aunque se salten
// secciones (las saltadas dicen por qué), y ia.py lo valida y lo resume.
func TestVonBenchSacaJSON(t *testing.T) {
	for _, b := range []string{"jq", "perl"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("sin " + b)
		}
	}
	dir := t.TempDir()
	k := filepath.Join(dir, "kling")
	if err := os.WriteFile(k, []byte(klingFalsoBanco), 0o755); err != nil {
		t.Fatal(err)
	}
	salida := filepath.Join(dir, "von.json")
	cmd := exec.Command("bash", "96-von-bench.sh", "von-falso")
	cmd.Env = append(os.Environ(), "KLING="+k, "KFALSO="+filepath.Join(dir, "estado"), "JSON_OUT="+salida,
		"RUNS=2", "REPLICAS=2", "CYCLES=2", "GATEWAY=0", "DROP_CACHES=0", "IA_BENCH=/nonexistent", "KFALSO_DISCO=*-p1-?c-0",
		"SECCIONES=restaurar primera memoria ociosa carga")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("96-von-bench.sh: %v\n%s", err, out)
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(leer(t, salida)), &j); err != nil {
		t.Fatalf("JSON inválido: %v\n%s", err, leer(t, salida))
	}
	for _, c := range []string{"esquema", "modelo", "host", "frio", "restaurar", "primera", "tps", "memoria", "ociosa", "semillas", "carga"} {
		if _, ok := j[c]; !ok {
			t.Errorf("falta la clave %q", c)
		}
	}
	pr, _ := j["primera"].(map[string]any)
	for _, c := range []string{"restaurar_caliente", "restaurar_fria", "descongelar_caliente", "descongelar_fria"} {
		if _, ok := pr[c]; !ok {
			t.Errorf("falta primera.%s: %v", c, pr)
		}
	}
	mem, _ := j["memoria"].(map[string]any)
	for _, c := range []string{"restauradas", "descongeladas", "remove"} {
		if _, ok := mem[c]; !ok {
			t.Errorf("falta memoria.%s: %v", c, mem)
		}
	}
	if des, _ := mem["descongeladas"].([]any); len(des) != 2 {
		t.Errorf("memoria.descongeladas: quería un punto por réplica (2), hay %v", mem["descongeladas"])
	}
	if s, _ := j["semillas"].(map[string]any); s["omitida"] == nil {
		t.Errorf("una sección no pedida dice que se omitió: %v", j["semillas"])
	}
	if c, _ := j["carga"].(map[string]any); c["omitida"] == nil {
		t.Errorf("la carga sin gateway (GATEWAY=0) dice que se omitió: %v", j["carga"])
	}
	// Sin restos: el banco borra todas sus máquinas.
	if ms, _ := filepath.Glob(filepath.Join(dir, "estado", "m", "*")); len(ms) != 0 {
		t.Errorf("quedaron máquinas: %v", ms)
	}
	// El primer intento de la primera restauración caliente leyó disco (KFALSO_DISCO): se aparta y se
	// repite. La primera descongelada caliente también leyó, y se queda: una réplica congelada siempre
	// lee su propio volcado.
	if ap, _ := pr["calientes_con_disco"].(map[string]any); ap == nil {
		t.Errorf("falta primera.calientes_con_disco: %v", pr)
	} else if a, d := len(ap["restaurar_caliente"].([]any)), len(ap["descongelar_caliente"].([]any)); a != 1 || d != 0 {
		t.Errorf("quería 1 restauración caliente apartada por leer disco y ninguna descongelada, hay %d y %d: %v", a, d, ap)
	}
	if dc, _ := pr["descongelar_caliente"].([]any); len(dc) != 2 {
		t.Errorf("quería las 2 descongeladas calientes, con su disco, hay %v", pr["descongelar_caliente"])
	} else if m, _ := dc[0].(map[string]any); m["disco_leido_bytes"] != float64(4096) {
		t.Errorf("la primera descongelada caliente leyó 4096 bytes: %v", dc[0])
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return
	}
	// Sin corrida.json (journal, desglose, service.log) ia.py rechaza la corrida.
	if out, err := exec.Command("python3", "-I", "bench/ia.py", "--validar", salida).CombinedOutput(); err == nil || !strings.Contains(string(out), "corrida.json") {
		t.Errorf("ia.py --validar sin corrida.json debía rechazarla: %v\n%s", err, out)
	}
	corridaFalsa(t, dir, "von-falso")
	if out, err := exec.Command("python3", "-I", "bench/ia.py", "--validar", salida).CombinedOutput(); err != nil {
		t.Errorf("ia.py --validar: %v\n%s", err, out)
	}
	if out, err := exec.Command("python3", "-I", "bench/ia.py", salida).CombinedOutput(); err != nil || !strings.Contains(string(out), "von-falso") {
		t.Errorf("ia.py resumen: %v\n%s", err, out)
	}
}

// gatewayFalso hace de `kling ai serve`: escucha en el socket, crea réplicas
// (ficheros en $KFALSO/m con el prefijo del gateway) hasta max_replicas y un
// segador las congela o las borra tras -idle según el "idle" del modelo.
const gatewayFalso = `import json, os, socketserver, sys, threading, time
from http.server import BaseHTTPRequestHandler
args = sys.argv[1:]
def opt(n, d=None):
    return args[args.index(n) + 1] if n in args else d
cfg = json.load(open(opt("-config")))
sock, pref = opt("-socket"), opt("-name-prefix")
idle = float(opt("-idle", "5s").rstrip("s"))
(nombre, m), = cfg["models"].items()
pol, maxr = m.get("idle") or "freeze", m.get("max_replicas") or int(opt("-max-replicas", "2"))
D = os.path.join(os.environ["KFALSO"], "m")
os.makedirs(D, exist_ok=True)
mu, uso, vuelo, cuenta = threading.Lock(), {}, {}, [0]
def estado(r):
    try:
        return open(os.path.join(D, r)).read().strip()
    except OSError:
        return None
def poner(r, e):
    open(os.path.join(D, r), "w").write(e + "\n")
def mias():
    return sorted(f for f in os.listdir(D) if f.startswith(pref))
def tomar():
    while True:
        with mu:
            ms = mias()
            r = next((x for x in ms if estado(x) == "running" and not vuelo.get(x)), None)
            r = r or next((x for x in ms if estado(x) == "frozen"), None)
            if r is None and len(ms) < maxr:
                cuenta[0] += 1
                r = "%s%d" % (pref, cuenta[0])
            if r:
                poner(r, "running")
                vuelo[r] = True
                return r
        time.sleep(0.01)
def soltar(r):
    with mu:
        vuelo[r], uso[r] = False, time.time()
def segador():
    while True:
        time.sleep(0.1)
        with mu:
            for r in mias():
                if estado(r) == "running" and not vuelo.get(r) and time.time() - uso.get(r, 0) > idle:
                    if pol == "remove":
                        os.remove(os.path.join(D, r))
                    else:
                        poner(r, "frozen")
class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def responder(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def do_GET(self):
        self.responder(200 if self.path == "/healthz" else 404, {})
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if not self.path.startswith("/v1/m/%s/" % nombre):
            return self.responder(404, {"error": "ruta"})
        r = tomar()
        time.sleep(0.02)
        soltar(r)
        self.responder(200, {"replica": r})
class S(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
threading.Thread(target=segador, daemon=True).start()
S(sock, H).serve_forever()
`

// Las partes de gateway del banco (remove, ociosa por gateway y carga) con un
// gateway de mentira: el segador del falso borra o congela según el "idle" que
// el banco escribe en su ai.json, y el JSON lo refleja.
func TestVonBenchConGatewayFalso(t *testing.T) {
	for _, b := range []string{"jq", "perl", "python3", "curl", "go"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("sin " + b)
		}
	}
	dir, err := os.MkdirTemp("", "vbg") // corto: la ruta del socket Unix tiene tope
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	iab := filepath.Join(dir, "ia-bench")
	if out, err := exec.Command("go", "build", "-o", iab, "./ia-bench").CombinedOutput(); err != nil {
		t.Fatalf("go build ia-bench: %v\n%s", err, out)
	}
	k, gw := filepath.Join(dir, "kling"), filepath.Join(dir, "gw.py")
	if err := os.WriteFile(k, []byte(klingFalsoBanco), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gw, []byte(gatewayFalso), 0o644); err != nil {
		t.Fatal(err)
	}
	salida := filepath.Join(dir, "von.json")
	cmd := exec.Command("bash", "96-von-bench.sh", "von-falso")
	cmd.Env = append(os.Environ(), "KLING="+k, "KFALSO="+filepath.Join(dir, "e"), "KFALSO_GW="+gw, "JSON_OUT="+salida,
		"TMPDIR="+dir, "REPLICAS=2", "CYCLES=2", "IDLE_S=1", "CARGA_S=1", "CLIENTES=1 2", "IA_BENCH="+iab,
		"DROP_CACHES=0", "SECCIONES=memoria ociosa carga")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("96-von-bench.sh: %v\n%s", err, out)
	}
	var j struct {
		Memoria struct {
			Remove []struct {
				Replicas int `json:"replicas"`
				Vivas    int `json:"vivas"`
				Dormidas struct {
					Estado string `json:"estado"`
				} `json:"dormidas"`
			} `json:"remove"`
		} `json:"memoria"`
		Ociosa struct {
			Gateway struct {
				Freeze []struct {
					MS       float64 `json:"ms"`
					Dormidas struct {
						Estado string `json:"estado"`
					} `json:"dormidas"`
				} `json:"idle_freeze"`
				Remove []struct {
					Dormidas struct {
						Estado string `json:"estado"`
					} `json:"dormidas"`
				} `json:"idle_remove"`
			} `json:"gateway"`
		} `json:"ociosa"`
		Carga struct {
			Ruta    string `json:"ruta"`
			Gateway struct {
				Puntos []struct {
					Clientes, N, Errores int
				} `json:"puntos"`
			} `json:"gateway"`
		} `json:"carga"`
	}
	if err := json.Unmarshal([]byte(leer(t, salida)), &j); err != nil {
		t.Fatalf("%v\n%s", err, leer(t, salida))
	}
	if r := j.Memoria.Remove; len(r) != 2 || r[1].Vivas != 2 || r[0].Dormidas.Estado != "borradas" || r[1].Dormidas.Estado != "borradas" {
		t.Errorf("memoria.remove: %+v\n%s", r, out)
	}
	g := j.Ociosa.Gateway
	if len(g.Freeze) != 2 || len(g.Remove) != 2 || g.Freeze[0].Dormidas.Estado != "congeladas" || g.Remove[0].Dormidas.Estado != "borradas" || g.Freeze[0].MS <= 0 {
		t.Errorf("ociosa.gateway: %+v", g)
	}
	if c := j.Carga; c.Ruta != "/v1/m/von-falso/v1/chat/completions" || len(c.Gateway.Puntos) != 2 || c.Gateway.Puntos[1].Errores != 0 || c.Gateway.Puntos[1].N == 0 {
		t.Errorf("carga: %+v", c)
	}
	if ms, _ := filepath.Glob(filepath.Join(dir, "e", "m", "*")); len(ms) != 0 {
		t.Errorf("quedaron máquinas: %v", ms)
	}
}

func TestIAPyAutoprueba(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("sin python3")
	}
	if out, err := exec.Command("python3", "-I", "bench/ia.py", "--self-test").CombinedOutput(); err != nil {
		t.Fatalf("ia.py --self-test: %v\n%s", err, out)
	}
}
