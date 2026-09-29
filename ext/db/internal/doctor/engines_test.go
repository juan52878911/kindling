package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// kFalsoRd es un kling cuya copia habla Redis: contesta al cliente del
// administrador según el comando que llega por stdin.
type kFalsoRd struct {
	machine api.Machine
	cmds    map[string]string // comando -> salida
	stdin   []string
}

func (f *kFalsoRd) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	switch {
	case len(args) == 2 && args[0] == "inspect":
		return json.Marshal(f.machine)
	case len(args) > 3 && args[0] == "exec" && args[len(args)-1] == redisAdmin:
		if args[2] != f.machine.ID || stdin == nil {
			return nil, fmt.Errorf("exec raro %q", args)
		}
		b, _ := io.ReadAll(stdin)
		f.stdin = append(f.stdin, string(b))
		if out, ok := f.cmds[strings.TrimSpace(string(b))]; ok {
			return []byte(out + "\n"), nil
		}
		return nil, fmt.Errorf("sin salida grabada para %q", b)
	}
	return nil, fmt.Errorf("kling inesperado %q", args)
}

// copiaRd es una copia Redis lista con la clave rotada y bien guardada.
func copiaRd(t *testing.T) *kFalsoRd {
	t.Helper()
	estado(t, map[string]string{
		"rdgold/password":          goldenPW + "\n",
		"rdgold/conn.env":          "ENGINE=redis\nDBUSER=app\n",
		"copies/m-1a2b3c/password": copyPW + "\n",
	})
	return &kFalsoRd{
		machine: maquina(map[string]string{LabelGolden: "rdgold", LabelState: StateReady, LabelEngine: "redis"}),
		cmds: map[string]string{
			"ACL LIST": "user app on sanitize-payload #" + sha256Hex(copyPW) + " ~* &* +@all -@admin\n" +
				"user default on sanitize-payload #" + sha256Hex("admin") + " ~* &* +@all",
			"ACL DRYRUN app CONFIG GET maxmemory": "This user has no permissions to run the 'config|get' command",
			"CLIENT LIST":                         "id=7 addr=/run/redis/redis.sock:0 fd=8 name= age=0 cmd=client|list user=default",
		},
	}
}

func correrK(t *testing.T, k interface {
	Run(context.Context, io.Reader, ...string) ([]byte, error)
}) (int, string) {
	t.Helper()
	var out bytes.Buffer
	n, err := Run(context.Background(), k, Target{Machine: "copia"}, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Log("\n" + out.String())
	return n, out.String()
}

// Una copia Redis recién preparada: sin problemas, y ningún comando lleva
// una contraseña.
func TestRdCopiaLimpia(t *testing.T) {
	k := copiaRd(t)
	n, out := correrK(t, k)
	if n != 0 {
		t.Fatalf("problemas: %d", n)
	}
	for _, r := range []string{"RD001", "RD002", "RD003", "RD051", "RD052", "RD053", "RD054"} {
		noTiene(t, out, r)
	}
	for _, s := range k.stdin {
		if strings.Contains(s, copyPW) || strings.Contains(s, goldenPW) {
			t.Fatal("un comando lleva una contraseña")
		}
	}
}

// Lo que se tuerce: la clave del dorado sigue, un usuario nopass, otro de
// más, la aplicación con comandos de administración y conexiones abiertas.
func TestRdCopiaConProblemas(t *testing.T) {
	k := copiaRd(t)
	k.cmds["ACL LIST"] = "user app on #" + sha256Hex(goldenPW) + " ~* &* +@all\n" +
		"user default on nopass ~* &* +@all\n" +
		"user dev\x1b[31m on #" + sha256Hex("x") + " ~* +@all\n" +
		"user viejo off nopass ~* +@all"
	k.cmds["ACL DRYRUN app CONFIG GET maxmemory"] = "OK"
	k.cmds["CLIENT LIST"] = "id=7 cmd=client|list\nid=8 cmd=get\nid=9 cmd=get"
	n, out := correrK(t, k)
	for _, c := range [][2]string{{"CRITICAL", "RD052"}, {"CRITICAL", "RD001"}, {"HIGH", "RD002"}, {"HIGH", "RD054"}, {"WARN", "RD003"}, {"INFO", "RD051"}} {
		tiene(t, out, c[0], c[1])
	}
	if n < 5 {
		t.Errorf("problemas: %d", n)
	}
	if strings.Contains(out, "\x1b") {
		t.Error("el informe lleva una secuencia de escape de la base")
	}
	if strings.Contains(out, `"viejo"`) {
		t.Error("un usuario apagado no es un riesgo")
	}
	if !strings.Contains(out, "2 client connection(s)") {
		t.Error("la conexión del doctor no cuenta")
	}
}

// Sin el fichero del host, sin el usuario de la app o en preparación.
func TestRdCopiaSinFicheroNiUsuario(t *testing.T) {
	k := copiaRd(t)
	k.machine.ID = "m-otra"
	k.machine.Labels[LabelState] = StatePreparing
	k.cmds["ACL LIST"] = "user default on #" + sha256Hex("admin") + " ~* &* +@all"
	_, out := correrK(t, k)
	tiene(t, out, "HIGH", "RD053")
	tiene(t, out, "HIGH", "RD052")

	k = copiaRd(t)
	k.machine.ID = "m-otra"
	_, out = correrK(t, k)
	tiene(t, out, "HIGH", "RD054")

	k = copiaRd(t)
	k.cmds["ACL LIST"] = "user app on #" + sha256Hex(copyPW) + " #" + sha256Hex("otra") + " ~* +@all -@admin\nuser default on #" + sha256Hex("admin") + " ~* +@all"
	_, out = correrK(t, k)
	tiene(t, out, "HIGH", "RD052")
}

// kFalsoSq es un kling cuya copia es SQLite: quick_check y stat.
type kFalsoSq struct {
	machine api.Machine
	check   string
	mode    string
	args    [][]string
}

func (f *kFalsoSq) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.args = append(f.args, args)
	switch {
	case len(args) == 2 && args[0] == "inspect":
		return json.Marshal(f.machine)
	case args[0] == "exec" && args[len(args)-1] == sqliteOpen("appdb"):
		b, _ := io.ReadAll(stdin)
		if strings.TrimSpace(string(b)) != "PRAGMA quick_check;" {
			return nil, fmt.Errorf("sql raro %q", b)
		}
		return []byte(f.check + "\n"), nil
	case args[0] == "exec" && args[len(args)-4] == "stat":
		return []byte(f.mode + "\n"), nil
	}
	return nil, fmt.Errorf("kling inesperado %q", args)
}

func copiaSq(t *testing.T) *kFalsoSq {
	t.Helper()
	estado(t, map[string]string{"sqgold/conn.env": "ENGINE=sqlite\nDBNAME=appdb\n"})
	return &kFalsoSq{
		machine: maquina(map[string]string{LabelGolden: "sqgold", LabelState: StateReady, LabelEngine: "sqlite"}),
		check:   "ok",
		mode:    "600",
	}
}

func TestSqCopia(t *testing.T) {
	k := copiaSq(t)
	n, out := correrK(t, k)
	if n != 0 {
		t.Fatalf("problemas: %d", n)
	}
	tiene(t, out, "INFO", "SQ050")
	for _, a := range k.args {
		if j := strings.Join(a, " "); strings.Contains(j, "sqlite3") && !strings.Contains(j, "-readonly") {
			t.Fatalf("el doctor abre la base con escritura: %v", a)
		}
	}

	k = copiaSq(t)
	k.check = "*** in database main ***\nPage 5: btreeInitPage() returns error code 11\x1b[2J"
	k.mode = "644"
	k.machine.Labels[LabelState] = StatePreparing
	n, out = correrK(t, k)
	tiene(t, out, "HIGH", "SQ001")
	tiene(t, out, "WARN", "SQ002")
	tiene(t, out, "HIGH", "SQ053")
	if n != 3 || strings.Contains(out, "\x1b") {
		t.Fatalf("problemas %d\n%s", n, out)
	}
}
