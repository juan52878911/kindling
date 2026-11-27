package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// P3 de la ronda 8 (bench/remoto/chispa-p3.sh) en seco: preparar y medir con
// `kling`, `docker`, `curl` y el python del venv falsos, sobre un checkout de
// chispa de mentira. Comprueba el orden de las piezas, que la clave no entra en
// la imagen ni en el dorado, que el gateway del banco la recibe por `auth` y que
// el JSON final es el de 96-von-bench.sh (ia.py lo valida).

const dockerFalsoP3 = `#!/usr/bin/env bash
echo "$*" >> "$FD/docker.log"
case $1 in
run)
  gm=""
  while [ $# -gt 0 ]; do
    case $1 in -v) case $2 in *:/gomodcache) gm=${2%%:/gomodcache} ;; esac; shift ;; esac
    shift
  done
  [ -n "$gm" ] || { echo "docker falso: run sin /gomodcache" >&2; exit 1; }
  d="$gm/cache/download/github.com/yalue/onnxruntime_go/@v"
  mkdir -p "$d" "$gm/cache/download/sumdb/sum.golang.org"
  echo v1.38.0 > "$d/list"; echo zip > "$d/v1.38.0.zip"; : > "$d/v1.38.0.lock" ;;
build)
  ctx=${!#}
  if [ -f "$ctx/config-claves.json" ]; then cp -R "$ctx" "$FD/ctx-visto"; fi ;;
save) [ "$2" = -o ] && echo tar > "$3" ;;
create) echo cid0 ;;
cp)
  case $2 in
  *chispa-server) printf '#!/bin/sh\nexec sleep 60\n' > "$3" ;;
  *) echo lib > "$3" ;;
  esac ;;
rm) ;;
image) echo 123456789 ;;
*) echo "docker falso: no sé $*" >&2; exit 1 ;;
esac
`

const pyFalsoP3 = `#!/usr/bin/env bash
echo "$*" >> "$FD/py.log"
[ "$1" = -I ] && shift
case $1 in
-c)
  s="$5/models--intfloat--multilingual-e5-small/snapshots/$4"
  mkdir -p "$s"; echo pesos > "$s/model.safetensors"; printf -- '---\nlicense: mit\n---\n' > "$s/README.md"
  echo "$s" ;;
model/alt/exportar_e5_onnx.py)
  while [ $# -gt 0 ]; do [ "$1" = --salida ] && { mkdir -p "$2"; echo onnx > "$2/e5.onnx"; }; shift; done ;;
runtime/tok/exportar.py) echo tok > "$3" ;;
-m)
  [ "${FALLA_BANCO:-0}" = 1 ] && { echo "banco roto" >&2; exit 1; }
  t=""; o=""
  while [ $# -gt 0 ]; do case $1 in --tarea) t=$2 ;; --salida) o=$2 ;; esac; shift; done
  mkdir -p "$o"; echo npy > "$o/banco-$t.npy"; echo '{"etiqueta":"odio","es_oro":true}' > "$o/banco-$t.jsonl" ;;
*) echo "python falso: no sé $*" >&2; exit 1 ;;
esac
`

const klingFalsoP3 = `#!/usr/bin/env bash
echo "$*" >> "$FD/kling.log"
S="$FD/snaps.json"; [ -f "$S" ] || echo '[]' > "$S"
case "$1 $2" in
"image import") ;;
"snapshots -json") cat "$S" ;;
"ai model")
  [ "$3" = deploy ] || { echo "kling falso: no sé $*" >&2; exit 1; }
  n=$4; cfg=""; a=("$@")
  for ((i = 0; i < ${#a[@]}; i++)); do [ "${a[i]}" = -config ] && cfg=${a[i+1]}; done
  v=$(jq --arg n "$n" '[.[] | select(.name | startswith($n + "-v"))] | length + 1' "$S")
  jq --arg s "$n-v$v" '. + [{name: $s, created_at: ("2026-11-23T00:00:0" + (length|tostring) + "Z")}]' "$S" > "$S.n" && mv "$S.n" "$S"
  jq --arg n "$n" --arg s "$n-v$v" 'if .models[$n] then . else .models[$n] = {kind:"http", snapshot:$s, port:8090, contract:"chispa-decide", health:{path:"/healthz"}, max_inflight:16} end' \
    "$cfg" > "$cfg.n" && mv "$cfg.n" "$cfg" ;;
*) echo "kling falso: no sé $*" >&2; exit 1 ;;
esac
`

const curlFalsoP3 = `#!/usr/bin/env bash
echo "$*" >> "$FD/curl.log"
case "$*" in *--data-binary*) cat > /dev/null ;; esac
exit 0
`

// vbFalso hace de 96-von-bench.sh: apunta su entorno y saca un JSON von-bench/2
// mínimo con lo que se le pidió.
const vbFalso = `#!/usr/bin/env bash
echo "snap=$1 secciones=$SECCIONES token=$TOKEN_FILE bloque=$GW_BLOQUE_FILE rep=${CARGA_REPLICAS:-} directo=${LOAD_DIRECT_URL:-} contrato=$CONTRATO cuerpo=$REQ_BODY" >> "$FD/vb.log"
[ -n "$GW_BLOQUE_FILE" ] && cat "$GW_BLOQUE_FILE" >> "$FD/bloques.log" && echo >> "$FD/bloques.log"
om='{"omitida":"no pedida"}'
carga=$om
if [ "$SECCIONES" = carga ]; then
  d=null
  [ -n "${LOAD_DIRECT_URL:-}" ] && d='{"puntos":[{"clientes":1,"rps":120,"p50_ms":4,"p95_ms":5,"p99_ms":6,"errores":0}]}'
  carga=$(jq -nc --argjson d "$d" '{ruta:"/v1/m/x/v1/decide", gateway:{puntos:[{clientes:1, rps:100, p50_ms:5, p95_ms:6, p99_ms:7, errores:0}]}, directo:$d}')
fi
jq -n --arg s "$1" --argjson c "$carga" --argjson om "$om" '{esquema:"von-bench/2",
  modelo:{snapshot:$s, contrato:"chispa-decide", mem_mib:3072, snapshot_mem_mib:2000},
  host:{uname:"falso", drop_caches:false, secciones:[]},
  frio:{arranque_ms:900, hasta_primera_ms:4000},
  restaurar:{thaw_ms:[10], hasta_primera_ms:[60], peticion_ms:[12]},
  primera:{restaurar_caliente:[{total_ms:60, peticion_ms:12, disco_leido_bytes:0}], restaurar_fria:$om, descongelar_caliente:[{total_ms:40, peticion_ms:10, disco_leido_bytes:0}], descongelar_fria:$om, umbral_caliente_bytes:0, calientes_con_disco:{restaurar_caliente:[], descongelar_caliente:[]}},
  tps:$om, memoria:{restauradas:[{replicas:1, pss_total_mib:2000, pss_mib:[2000], rss_total_mib:2100, host:{}}], descongeladas:[], remove:$om},
  ociosa:$om, arranque:$om, secreto:$om, semillas:$om, carga:$c}' > "$JSON_OUT"
`

// chispaFalsa arma un checkout de chispa con lo que chispa-p3.sh lee de él.
func chispaFalsa(t *testing.T, dir string) (string, string) {
	t.Helper()
	src := filepath.Join(dir, "chispa")
	hex64 := strings.Repeat("ab", 32)
	ficheros := map[string]string{
		"deploy/kindling/Dockerfile": "ARG GO_VERSION=1.27\nARG GO_DIGEST=sha256:" + hex64 + "\nARG DEBIAN_DIGEST=sha256:" + hex64 +
			"\nARG ORT_NUPKG_SHA256=" + hex64 + "\n",
		"server/go.mod": "module github.com/juan52878911/chispa/server\n",
		"server/go.sum": "",
		"model/datos/fixtures/hatecheck/test.csv": "mhc_case_id,functionality,test_case,label_gold\n" +
			"spanish-1,derog,Detesto a los vecinos zurdos (plantilla 1).,hateful\n" +
			"spanish-3,ident,\"Respeto a los vecinos, zurdos (plantilla 2).\",non-hateful\n",
	}
	for f, c := range ficheros {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", src, "-c", "user.email=p3@banco", "-c", "user.name=p3", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	g("init", "-q")
	g("add", ".")
	g("commit", "-q", "-m", "chispa de mentira")
	return src, g("rev-parse", "HEAD")
}

func correrP3(t *testing.T, dir string, env []string, orden string) string {
	t.Helper()
	cmd := exec.Command("bash", "bench/remoto/chispa-p3.sh", orden)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chispa-p3.sh %s: %v\n%s", orden, err, out)
	}
	if testing.Verbose() {
		t.Log(string(out))
	}
	return string(out)
}

func entornoP3(t *testing.T, falla bool) (string, []string) {
	t.Helper()
	for _, b := range []string{"jq", "perl", "git", "python3", "sha256sum", "timeout"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("sin " + b)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range map[string]string{"docker": dockerFalsoP3, "py": pyFalsoP3, "kling": klingFalsoP3, "curl": curlFalsoP3, "vb": vbFalso} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src, commit := chispaFalsa(t, dir)
	env := []string{"FD=" + dir, "KB=" + dir, "P3_DIR=" + filepath.Join(dir, "p3"), "RES=" + filepath.Join(dir, "res"),
		"KLING=" + filepath.Join(bin, "kling"), "DOCKER=" + filepath.Join(bin, "docker"), "P3_PY=" + filepath.Join(bin, "py"),
		"VB=" + filepath.Join(bin, "vb"), "CHISPA_SRC=" + src, "CHISPA_COMMIT=" + commit, "P3_APT=0", "RUNS=2"}
	if falla {
		env = append(env, "FALLA_BANCO=1")
	}
	return dir, env
}

func TestChispaP3PrepararYMedir(t *testing.T) {
	dir, env := entornoP3(t, false)
	p3 := filepath.Join(dir, "p3")
	correrP3(t, dir, env, "preparar")

	clave := strings.TrimSpace(leer(t, filepath.Join(p3, "chispa.key")))
	if len(clave) != 64 {
		t.Fatalf("clave de %d caracteres, quería 64 hex", len(clave))
	}
	if fi, _ := os.Stat(filepath.Join(p3, "chispa.key")); fi.Mode().Perm() != 0o600 {
		t.Errorf("chispa.key es %04o, quería 0600", fi.Mode().Perm())
	}
	// En la imagen va el hash, nunca la clave.
	h := sha256.Sum256([]byte(clave))
	cfg := leer(t, filepath.Join(p3, "ctx", "config-claves.json"))
	if !strings.Contains(cfg, hex.EncodeToString(h[:])) {
		t.Errorf("config-claves.json no lleva el sha256 de la clave:\n%s", cfg)
	}
	if strings.Contains(leer(t, filepath.Join(p3, "ctx", "config-abierta.json")), "api_keys") {
		t.Error("la config abierta lleva api_keys")
	}
	filepath.Walk(filepath.Join(dir, "ctx-visto"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(leer(t, p), clave) {
			t.Errorf("la clave está en el contexto de la imagen: %s", p)
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(dir, "ctx-visto")); err != nil {
		t.Errorf("no se construyó ninguna imagen hija: %v", err)
	}
	// gomod-proxy sin candados ni sumdb, como pide el README de chispa.
	gp := filepath.Join(dir, "chispa", "gomod-proxy")
	if _, err := os.Stat(filepath.Join(gp, "github.com/yalue/onnxruntime_go/@v/v1.38.0.zip")); err != nil {
		t.Errorf("gomod-proxy sin el módulo: %v", err)
	}
	if ls, _ := filepath.Glob(filepath.Join(gp, "github.com/yalue/onnxruntime_go/@v/*.lock")); len(ls) != 0 {
		t.Errorf("gomod-proxy con candados: %v", ls)
	}
	dl := leer(t, filepath.Join(dir, "docker.log"))
	for _, want := range []string{"golang:1.27-bookworm@sha256:", "--target imagen-ort", "--network host", "save -o"} {
		if !strings.Contains(dl, want) {
			t.Errorf("docker no recibió %q:\n%s", want, dl)
		}
	}
	// El dorado con claves no se calienta (el warmup no lleva cabeceras); el abierto sí.
	kl := leer(t, filepath.Join(dir, "kling.log"))
	for _, l := range strings.Split(kl, "\n") {
		if strings.Contains(l, "deploy chispa-p3 ") && strings.Contains(l, "-warmup") {
			t.Errorf("el dorado con claves se calentó: %s", l)
		}
		if strings.Contains(l, "deploy chispa-p3a ") && !strings.Contains(l, "-warmup") {
			t.Errorf("el dorado abierto no se calentó: %s", l)
		}
	}
	if !strings.Contains(kl, "image import -archive") {
		t.Errorf("no importó la imagen:\n%s", kl)
	}
	var ai struct {
		Models map[string]struct {
			Snapshot string `json:"snapshot"`
			Auth     *struct {
				Header, Scheme, File string
			} `json:"auth"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(leer(t, filepath.Join(p3, "ai.json"))), &ai); err != nil {
		t.Fatal(err)
	}
	if m := ai.Models["chispa-p3"]; m.Snapshot != "chispa-p3-v1" || m.Auth == nil || m.Auth.File != filepath.Join(p3, "chispa.key") {
		t.Errorf("bloque chispa-p3: %+v", m)
	}
	if m := ai.Models["chispa-p3a"]; m.Snapshot != "chispa-p3a-v1" || m.Auth != nil {
		t.Errorf("bloque chispa-p3a: %+v", m)
	}
	if strings.Contains(leer(t, filepath.Join(p3, "peticion.json")), "examples") {
		t.Error("con banco, la petición no debe llevar examples[]")
	}
	if n := strings.Count(leer(t, filepath.Join(p3, "warmup.jsonl")), "\n"); n != 3 {
		t.Errorf("warmup.jsonl con %d peticiones, quería 3", n)
	}
	var prep map[string]any
	if err := json.Unmarshal([]byte(leer(t, filepath.Join(dir, "res", "p3-preparado.json"))), &prep); err != nil {
		t.Fatalf("p3-preparado.json: %v", err)
	}
	if et, _ := prep["etapas"].([]any); len(et) != 11 {
		t.Errorf("quería 11 etapas cronometradas, hay %v", prep["etapas"])
	}

	correrP3(t, dir, env, "medir")
	vb := leer(t, filepath.Join(dir, "vb.log"))
	lineas := strings.Split(strings.TrimSpace(vb), "\n")
	if len(lineas) != 6 { // por variante: secciones + carga con 1 réplica + carga con 4
		t.Fatalf("96-von-bench se llamó %d veces, quería 6:\n%s", len(lineas), vb)
	}
	for _, l := range lineas {
		claves := strings.Contains(l, "snap=chispa-p3-v1 ")
		if tiene := strings.Contains(l, "token="+filepath.Join(p3, "chispa.key")); tiene != claves {
			t.Errorf("TOKEN_FILE mal puesto: %s", l)
		}
		if !strings.Contains(l, "contrato=chispa-decide") || !strings.Contains(l, "cuerpo=@"+filepath.Join(p3, "peticion.json")) {
			t.Errorf("contrato o cuerpo: %s", l)
		}
		if strings.Contains(l, "rep=1 ") != strings.Contains(l, "directo=http://127.0.0.1:18090/v1/decide") {
			t.Errorf("el directo va solo con la tanda de 1 réplica: %s", l)
		}
	}
	if b := leer(t, filepath.Join(dir, "bloques.log")); !strings.Contains(b, `"auth"`) || !strings.Contains(b, `"max_inflight":4`) {
		t.Errorf("bloques del gateway del banco sin auth o sin la tanda de max_inflight 4:\n%s", b)
	}
	corridaFalsa(t, filepath.Join(dir, "res"), "chispa-p3-v1") // lo que deja remoto/ia.sh alrededor de chispa-p3.sh
	for _, d := range []string{"chispa-p3-v1", "chispa-p3a-v1"} {
		f := filepath.Join(dir, "res", "ia-p3-"+d+".json")
		var j map[string]any
		if err := json.Unmarshal([]byte(leer(t, f)), &j); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c, _ := j["carga"].(map[string]any); c == nil || c["gateway"] == nil || c["directo"] == nil {
			t.Errorf("%s: la carga de la primera tanda (con el directo) no se juntó: %v", d, j["carga"])
		}
		if tandas, _ := j["carga_tandas"].(map[string]any); tandas["r4"] == nil {
			t.Errorf("%s: falta la tanda de 4 réplicas: %v", d, j["carga_tandas"])
		}
		if out, err := exec.Command("python3", "-I", "bench/ia.py", "--validar", f).CombinedOutput(); err != nil {
			t.Errorf("ia.py --validar %s: %v\n%s", d, err, out)
		}
	}
	var res []map[string]any
	if err := json.Unmarshal([]byte(leer(t, filepath.Join(dir, "res", "p3-resumen.json"))), &res); err != nil || len(res) != 2 {
		t.Fatalf("p3-resumen.json: %v %v", err, res)
	}
	if !strings.Contains(leer(t, filepath.Join(dir, "res", "p3-resumen.txt")), "[claves] chispa-p3-v1") {
		t.Errorf("p3-resumen.txt:\n%s", leer(t, filepath.Join(dir, "res", "p3-resumen.txt")))
	}
}

// Si generar_banco.py falla, P3 sigue: banco vacío y los ejemplos en la petición.
func TestChispaP3SinBanco(t *testing.T) {
	dir, env := entornoP3(t, true)
	out := correrP3(t, dir, append(env, "P3_VARIANTES=abierto"), "preparar")
	if !strings.Contains(out, "examples[]") {
		t.Errorf("no avisó del banco que falló:\n%s", out)
	}
	p3 := filepath.Join(dir, "p3")
	if !strings.Contains(leer(t, filepath.Join(p3, "peticion.json")), `"examples"`) {
		t.Error("sin banco, la petición tiene que llevar examples[]")
	}
	var c struct {
		Knn struct {
			Tareas map[string]any `json:"tareas"`
		} `json:"knn"`
	}
	if err := json.Unmarshal([]byte(leer(t, filepath.Join(p3, "ctx", "config-abierta.json"))), &c); err != nil || len(c.Knn.Tareas) != 0 {
		t.Errorf("sin banco, knn.tareas tiene que quedar vacío: %v %v", err, c.Knn.Tareas)
	}
	if kl := leer(t, filepath.Join(dir, "kling.log")); strings.Contains(kl, "chispa-p3 ") {
		t.Errorf("con P3_VARIANTES=abierto solo se despliega chispa-p3a:\n%s", kl)
	}
}

// 96-von-bench.sh con lo que le pasa P3: el bloque del ai.json de deploy
// (GW_BLOQUE_FILE) llega al gateway del banco con solo snapshot, idle y
// max_replicas tocados, CARGA_REPLICAS manda en la carga y TOKEN_FILE tiene
// que ser una ruta absoluta.
func TestVonBenchBloqueYClave(t *testing.T) {
	for _, b := range []string{"jq", "perl", "python3", "curl", "go"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("sin " + b)
		}
	}
	dir, err := os.MkdirTemp("", "vbp3") // corto: la ruta del socket Unix tiene tope
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	iab := filepath.Join(dir, "ia-bench")
	if out, err := exec.Command("go", "build", "-o", iab, "./ia-bench").CombinedOutput(); err != nil {
		t.Fatalf("go build ia-bench: %v\n%s", err, out)
	}
	k, gw, envol := filepath.Join(dir, "kling"), filepath.Join(dir, "gw.py"), filepath.Join(dir, "envol.py")
	copia := filepath.Join(dir, "ai-visto.json")
	for f, c := range map[string]string{k: klingFalsoBanco, gw: gatewayFalso,
		envol: "import os, shutil, sys\nshutil.copy(sys.argv[sys.argv.index('-config') + 1], os.environ['COPIA'])\n" +
			"sys.argv[0] = os.environ['GW_REAL']\nexec(compile(open(os.environ['GW_REAL']).read(), os.environ['GW_REAL'], 'exec'))\n"} {
		if err := os.WriteFile(f, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	clave := filepath.Join(dir, "chispa.key")
	if err := os.WriteFile(clave, []byte("secreto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bloque := filepath.Join(dir, "bloque.json")
	if err := os.WriteFile(bloque, []byte(`{"kind":"von","max_inflight":3,"auth":{"header":"Authorization","scheme":"Bearer","file":"`+clave+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	base := append(os.Environ(), "KLING="+k, "KFALSO="+filepath.Join(dir, "e"), "KFALSO_GW="+envol, "GW_REAL="+gw, "COPIA="+copia,
		"TMPDIR="+dir, "REPLICAS=3", "CARGA_REPLICAS=1", "CARGA_S=1", "CLIENTES=1", "IA_BENCH="+iab, "DROP_CACHES=0",
		"SECCIONES=carga", "GW_BLOQUE_FILE="+bloque, "TOKEN_FILE="+clave, "JSON_OUT="+filepath.Join(dir, "von.json"))
	cmd := exec.Command("bash", "96-von-bench.sh", "von-falso")
	cmd.Env = base
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("96-von-bench.sh: %v\n%s", err, out)
	}
	var ai struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(leer(t, copia)), &ai); err != nil {
		t.Fatal(err)
	}
	m := ai.Models["von-falso"]
	if m["snapshot"] != "von-falso" || m["idle"] != "remove" || m["max_replicas"] != float64(1) || m["max_inflight"] != float64(3) || m["auth"] == nil {
		t.Errorf("bloque del gateway del banco: %v", m)
	}
	cmd = exec.Command("bash", "96-von-bench.sh", "von-falso")
	cmd.Env = append(base, "TOKEN_FILE=chispa.key")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "absolute") {
		t.Errorf("un TOKEN_FILE relativo debería fallar: %v\n%s", err, out)
	}
}
