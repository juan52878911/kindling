package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/mysqlpw"
	"github.com/juan52878911/kindling/pkg/api"
)

// kFalsoMy es un kling cuya copia habla MySQL: contesta con salidas grabadas
// del cliente, reconociendo la consulta por su marca /* doctor:<nombre> */.
type kFalsoMy struct {
	machine api.Machine
	sql     map[string]string
	skew    time.Duration
	stdin   []string
}

func (f *kFalsoMy) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	switch {
	case len(args) == 2 && args[0] == "inspect":
		return json.Marshal(f.machine)
	case len(args) > 3 && args[0] == "exec" && args[len(args)-2] == "date":
		return []byte(strconv.FormatInt(time.Now().Add(f.skew).Unix(), 10) + "\n"), nil
	case len(args) > 3 && args[0] == "exec" && args[len(args)-1] == myClient:
		if args[2] != f.machine.ID || stdin == nil {
			return nil, fmt.Errorf("exec raro %q", args)
		}
		b, _ := io.ReadAll(stdin)
		f.stdin = append(f.stdin, string(b))
		m := reMarca.FindStringSubmatch(string(b))
		if m == nil {
			return nil, errors.New("sql sin marca")
		}
		if m[1] == "clock" {
			return fmt.Appendf(nil, "%.6f\n", float64(time.Now().Add(f.skew).UnixNano())/1e9), nil
		}
		if out, ok := f.sql[m[1]]; ok {
			return []byte(out + "\n"), nil
		}
		return nil, fmt.Errorf("sin salida grabada para %s", m[1])
	}
	return nil, fmt.Errorf("kling inesperado %q", args)
}

// copiaMy es una copia MySQL lista con la contraseña rotada y bien guardada.
func copiaMy(t *testing.T) *kFalsoMy {
	t.Helper()
	estado(t, map[string]string{
		"mygold/password":          goldenPW + "\n",
		"mygold/conn.env":          "ENGINE=mysql\nDBUSER=app\nDBNAME=appdb\n",
		"copies/m-1a2b3c/password": copyPW + "\n",
	})
	return &kFalsoMy{
		machine: maquina(map[string]string{LabelGolden: "mygold", LabelState: StateReady, LabelEngine: "mysql"}),
		sql: map[string]string{
			"version": `"11.4.3-MariaDB"`,
			"users": `[{"user":"root","host":"localhost","plugin":"mysql_native_password","auth":"invalid","locked":false,"role":false},` +
				`{"user":"mariadb.sys","host":"localhost","plugin":"mysql_native_password","auth":"","locked":true,"role":false},` +
				`{"user":"app","host":"%","plugin":"mysql_native_password","auth":"` + mysqlpw.NativeHash(copyPW) + `","locked":false,"role":false}]`,
			"privileges": `[{"grantee":"'root'@'localhost'","priv":"SUPER","grantable":"YES"},{"grantee":"'root'@'localhost'","priv":"FILE","grantable":"YES"}]`,
			"settings":   `{"local_infile": 0, "secure_file_priv": "/var/lib/mysql-files", "audit": "ACTIVE"}`,
			"definers":   `[]`,
			"clients":    `0`,
		},
	}
}

func correrMy(t *testing.T, k *kFalsoMy) (int, string) {
	t.Helper()
	var out bytes.Buffer
	n, err := Run(context.Background(), k, Target{Machine: "copia"}, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Log("\n" + out.String())
	return n, out.String()
}

// Una copia recién preparada por kling db: sin problemas (root local e
// informativo), y ninguna consulta lleva nada que no sea SQL fijo.
func TestMyCopiaLimpia(t *testing.T) {
	k := copiaMy(t)
	n, out := correrMy(t, k)
	if n != 0 {
		t.Fatalf("problemas: %d", n)
	}
	tiene(t, out, "INFO", "MY001")
	tiene(t, out, "INFO", "MY030")
	for _, r := range []string{"MY004", "MY052", "MY053", "MY054", "MY012"} {
		noTiene(t, out, r)
	}
	for _, s := range k.stdin {
		if strings.Contains(s, copyPW) || strings.Contains(s, goldenPW) {
			t.Fatal("una consulta lleva una contraseña")
		}
	}
}

// Lo que se tuerce: la clave del dorado sigue, una cuenta de red con SUPER,
// cuentas sin contraseña o anónimas, LOCAL INFILE, secure_file_priv vacío,
// sin registro de conexiones y una vista que corre como root.
func TestMyCopiaConProblemas(t *testing.T) {
	k := copiaMy(t)
	k.sql["users"] = `[{"user":"app","host":"%","plugin":"mysql_native_password","auth":"` + mysqlpw.NativeHash(goldenPW) + `","locked":false,"role":false},` +
		`{"user":"","host":"localhost","plugin":"","auth":"","locked":false,"role":false},` +
		`{"user":"dev","host":"%","plugin":"caching_sha2_password","auth":"","locked":false,"role":false},` +
		`{"user":"lector","host":"","plugin":"","auth":"","locked":false,"role":true}]`
	k.sql["privileges"] = `[{"grantee":"'app'@'%'","priv":"SUPER","grantable":"NO"},{"grantee":"'dev'@'%'","priv":"SELECT","grantable":"NO"}]`
	k.sql["settings"] = `{"local_infile": 1, "secure_file_priv": "", "audit": ""}`
	k.sql["definers"] = `[{"kind":"view","schema":"appdb","name":"v\u001b[31m","definer":"root@localhost"}]`
	k.sql["clients"] = `2`
	n, out := correrMy(t, k)
	for _, c := range [][2]string{{"CRITICAL", "MY052"}, {"CRITICAL", "MY001"}, {"CRITICAL", "MY003"}, {"CRITICAL", "MY004"},
		{"HIGH", "MY002"}, {"HIGH", "MY054"}, {"WARN", "MY010"}, {"WARN", "MY011"}, {"WARN", "MY012"}, {"WARN", "MY020"}, {"INFO", "MY051"}} {
		tiene(t, out, c[0], c[1])
	}
	if n < 10 {
		t.Errorf("problemas: %d", n)
	}
	if strings.Contains(out, "\x1b") {
		t.Error("el informe lleva una secuencia de escape de la base")
	}
	if strings.Contains(out, `"lector"`) {
		t.Error("un rol sin contraseña no es una cuenta")
	}
}

// Sin el fichero del host, sin el usuario de la app o en preparación.
func TestMyCopiaSinFicheroNiUsuario(t *testing.T) {
	k := copiaMy(t)
	k.machine.ID = "m-otra"
	k.machine.Labels[LabelState] = StatePreparing
	k.sql["users"] = `[]`
	_, out := correrMy(t, k)
	tiene(t, out, "HIGH", "MY053")
	tiene(t, out, "HIGH", "MY052")

	k = copiaMy(t)
	k.machine.ID = "m-otra"
	_, out = correrMy(t, k)
	tiene(t, out, "HIGH", "MY054")

	k = copiaMy(t)
	k.skew = time.Minute
	_, out = correrMy(t, k)
	tiene(t, out, "HIGH", "MY050")
}

// -url mysql:// se rechaza con un mensaje que dice cómo revisar una copia.
func TestMyURLNoAdmitida(t *testing.T) {
	_, err := parseURL("mysql://app@db:3306/appdb")
	if err == nil || !strings.Contains(err.Error(), "postgres only") {
		t.Fatalf("err = %v", err)
	}
}
