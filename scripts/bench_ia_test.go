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

// En metal no hay virtualización anidada que pedir: --cpu-options sobra (y
// AWS la rechaza en un .metal).
func TestFaseIAMetalSinAnidada(t *testing.T) {
	dir := t.TempDir()
	log := awsFalso(t, dir)
	out, err := ec2(t, dir, []string{"EC2_TYPE=c7i.metal-24xl", "TOPE_MIN=120"}, "--dry-run", "ia")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sinLlamadas(t, log)
	if strings.Contains(out, "NestedVirtualization") {
		t.Errorf("metal no lleva NestedVirtualization:\n%s", out)
	}
}

// El tope de gasto se comprueba antes de nada, también en seco: el peor caso
// (precio máximo de spot × tope de minutos) más lo ya apuntado en el libro no
// puede pasar de PRESUPUESTO_USD (10).
func TestFaseIARespetaElPresupuesto(t *testing.T) {
	t.Run("una sola instancia demasiado cara", func(t *testing.T) {
		dir := t.TempDir()
		log := awsFalso(t, dir)
		out, err := ec2(t, dir, []string{"EC2_TYPE=c7i.metal-24xl", "TOPE_MIN=720"}, "--dry-run", "ia")
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
		out, err := ec2(t, dir, []string{"EC2_TYPE=x9z.huge"}, "--dry-run", "ia")
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
	fs := []string{"96-von-bench.sh", "bench/ec2.sh", "bench/remoto/fases.sh", "bench/remoto/ia.sh"}
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
"top -json") printf '{"available_mib":1000,"machines":['; s=""; for f in "$D"/m/*; do [ -e "$f" ] || continue; [ "$(cat "$f")" = running ] || continue; printf '%s{"name":"%s","pid":%d,"pss_mib":10,"disk_read_bytes":4096,"state":"running"}' "$s" "$(basename "$f")" "$$"; s=,; done; echo ']}' ;;
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
		"RUNS=2", "REPLICAS=2", "CYCLES=2", "GATEWAY=0", "DROP_CACHES=0", "IA_BENCH=/nonexistent",
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
	if _, err := exec.LookPath("python3"); err != nil {
		return
	}
	if out, err := exec.Command("python3", "-I", "bench/ia.py", "--validar", salida).CombinedOutput(); err != nil {
		t.Errorf("ia.py --validar: %v\n%s", err, out)
	}
	if out, err := exec.Command("python3", "-I", "bench/ia.py", salida).CombinedOutput(); err != nil || !strings.Contains(string(out), "von-falso") {
		t.Errorf("ia.py resumen: %v\n%s", err, out)
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
