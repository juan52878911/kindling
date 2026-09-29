package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/askllm"
	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// reportKling: los exec de preparar una copia (pg_isready, la purga y la
// rotación) van al falso de siempre; los de ask, al de ask.
type reportKling struct{ *askKling }

func (k *reportKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" {
		return k.fakeKling.Run(ctx, stdin, args...)
	}
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	cmd := strings.Join(args, " ")
	if strings.Contains(cmd, "pg_isready") || strings.Contains(string(in), purgeMarker) || strings.Contains(string(in), " PASSWORD '") {
		return k.fakeKling.Run(ctx, strings.NewReader(string(in)), args...)
	}
	return k.askKling.Run(ctx, strings.NewReader(string(in)), args...)
}

// newReportApp: el app con los dos falsos y el proveedor falso en newProvider.
func newReportApp(t *testing.T, answers ...string) (*testApp, *reportKling, *fakeProvider) {
	t.Helper()
	ta := newTestApp(t)
	k := &reportKling{&askKling{fakeKling: ta.f, problems: "[]", schema: testSchema, csv: "name\nana\nluis\n"}}
	ta.app.k = k
	prov := &fakeProvider{answers: answers}
	old := newProvider
	newProvider = func(string, string, time.Duration) (askllm.Provider, error) { return prov, nil }
	t.Cleanup(func() { newProvider = old })
	return ta, k, prov
}

func testReportDef(t *testing.T, name, out string) *reportDef {
	t.Helper()
	o := askOpts{model: askllm.DefaultModel, limit: 200, timeout: 30 * time.Second, llmTime: 90 * time.Second}
	d, err := newReportDef(name, "pg", defaultOwner, "1w", "clientes nuevos", out, o, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestReportAddGuardaPrivado(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	d := testReportDef(t, "lunes", "")
	var out strings.Builder
	if err := addReport(&out, d, false); err != nil {
		t.Fatal(err)
	}
	p, _ := reportPath("lunes")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("definition: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Dir(p)); st.Mode().Perm() != 0o700 {
		t.Errorf("reports dir mode %v", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "never rows") || !strings.Contains(out.String(), "report run lunes -due") {
		t.Errorf("add output:\n%s", out.String())
	}
	if err := addReport(&out, d, false); err == nil {
		t.Fatal("add over an existing report without -replace")
	}
	if err := addReport(&out, d, true); err != nil {
		t.Fatal(err)
	}
	got, err := loadReport("lunes")
	if err != nil || got.Question != "clientes nuevos" || got.Golden != "pg" || got.Every != "1w" {
		t.Fatalf("%+v %v", got, err)
	}
	// Un fichero abierto a otros o tocado a mano no pasa.
	_ = os.Chmod(p, 0o644)
	if _, err := loadReport("lunes"); err == nil {
		t.Error("a world-readable definition was loaded")
	}
	_ = os.Chmod(p, 0o600)
	b, _ := os.ReadFile(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["send_data"] = true // sin explain: ask no lo admite
	b, _ = json.Marshal(m)
	_ = os.WriteFile(p, b, 0o600)
	if _, err := loadReport("lunes"); err == nil || !strings.Contains(err.Error(), "-send-data") {
		t.Errorf("a hand-edited definition skipped ask's checks: %v", err)
	}
	m["send_data"], m["api_key"] = false, "sk-x"
	b, _ = json.Marshal(m)
	_ = os.WriteFile(p, b, 0o600)
	if _, err := loadReport("lunes"); err == nil {
		t.Error("an unknown field was accepted")
	}
	var rm strings.Builder
	if err := reportRm(&rm, "lunes"); err != nil || strings.TrimSpace(rm.String()) != "lunes" {
		t.Fatalf("rm: %v %q", err, rm.String())
	}
	if _, err := loadReport("lunes"); err == nil {
		t.Error("still there after rm")
	}
}

func TestReportDefValida(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	o := askOpts{limit: 200, timeout: 30 * time.Second, llmTime: 90 * time.Second}
	now := time.Now()
	bad := []func() (*reportDef, error){
		func() (*reportDef, error) { return newReportDef("Bad", "pg", "local", "1w", "q", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "../pg", "local", "1w", "q", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "pg", "local", "weekly", "q", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "pg", "local", "0d", "q", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "pg", "local", "1w", " ", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "pg", "BAD", "1w", "q", "", o, now) },
		func() (*reportDef, error) { return newReportDef("r", "pg", "local", "1w", "q", t.TempDir(), o, now) },
		func() (*reportDef, error) {
			return newReportDef("r", "pg", "local", "1w", "q", "/nonexistent/dir/x", o, now)
		},
		func() (*reportDef, error) {
			o2 := o
			o2.explain = true // sin -send-data
			return newReportDef("r", "pg", "local", "1w", "q", "", o2, now)
		},
	}
	for i, f := range bad {
		if _, err := f(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	// Una salida relativa se guarda absoluta (cron corre en otro directorio).
	t.Chdir(t.TempDir())
	d, err := newReportDef("r", "pg", "local", "30m", "q", "out.txt", o, now)
	if err != nil || !filepath.IsAbs(d.Out) {
		t.Fatalf("%+v %v", d, err)
	}
	for s, want := range map[string]time.Duration{"30m": 30 * time.Minute, "6h": 6 * time.Hour, "1d": 24 * time.Hour, "2w": 14 * 24 * time.Hour} {
		if got, err := parseEvery(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
}

func TestReportRun(t *testing.T) {
	ta, k, prov := newReportApp(t, "```sql\nSELECT name FROM customers\n```")
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "informe.txt")
	d := testReportDef(t, "lunes", out)
	if err := addReport(io.Discard, d, false); err != nil {
		t.Fatal(err)
	}
	ta.now = func() time.Time { return time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC) }
	if err := ta.reportRun(ctx, d, d.Out, false); err != nil {
		t.Fatalf("%v\n%s", err, ta.err)
	}
	// La copia de la ejecución ya no está, ni su clave.
	for _, mc := range ta.f.machines {
		if mc.Name == reportCopyName("lunes") {
			t.Fatal("the report copy was left behind")
		}
	}
	var runID string
	for _, c := range ta.f.calls {
		if len(c.args) > 0 && c.args[0] == "run" {
			j := strings.Join(c.args, " ")
			if !strings.Contains(j, "-from pg -name rpt-lunes") || !strings.Contains(j, "-label "+labelReport+"=lunes") {
				t.Errorf("run: %s", j)
			}
		}
		if len(c.args) > 1 && c.args[0] == "rm" {
			runID = c.args[len(c.args)-1]
		}
	}
	if runID == "" {
		t.Fatal("the copy was not removed")
	}
	if _, err := os.Stat(passwordFile(runID)); err == nil {
		t.Error("the copy's password is still on the host")
	}
	// Resultado 0600 con la pregunta, las filas y la SQL; nada en stdout.
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("output: %v %v", st, err)
	}
	b, _ := os.ReadFile(out)
	for _, w := range []string{"Report lunes", "2026-09-28 07:00", "clientes nuevos", "ana", "luis", "(2 rows)", "SELECT name FROM customers", "kling_db_ro"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("output lacks %q:\n%s", w, b)
		}
	}
	if ta.out.Len() != 0 {
		t.Errorf("stdout: %s", ta.out)
	}
	// Las garantías de ask: la SQL va encerrada y al modelo no llega ninguna fila.
	assertEncerrada(t, k.askKling, defaultRORole, "SELECT name FROM customers", 200)
	for _, p := range prov.prompts {
		if strings.Contains(p, "ana") || strings.Contains(p, "luis") {
			t.Fatal("result rows reached the model without -send-data")
		}
	}
	if len(prov.prompts) != 1 {
		t.Errorf("%d calls to the model", len(prov.prompts))
	}

	// -due: acaba de correr, así que no hace nada.
	n := len(ta.f.calls)
	ta.err.Reset()
	if err := ta.reportRun(ctx, d, d.Out, true); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.calls) != n || !strings.Contains(ta.err.String(), "not due until 2026-10-05") {
		t.Fatalf("a run that was not due did something: %s", ta.err)
	}
	// Una semana después, sí.
	ta.now = func() time.Time { return time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC) }
	prov.answers = []string{"```sql\nSELECT name FROM customers\n```"}
	if err := ta.reportRun(ctx, d, d.Out, true); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.calls) == n {
		t.Fatal("a due run did nothing")
	}
}

func TestReportRunJSONYStdout(t *testing.T) {
	ta, _, _ := newReportApp(t, "```sql\nSELECT name FROM customers\n```")
	d := testReportDef(t, "r", "")
	d.JSON = true
	if err := ta.reportRun(context.Background(), d, "", false); err != nil {
		t.Fatal(err)
	}
	var m reportMeta
	if err := json.Unmarshal(ta.out.Bytes(), &m); err != nil || m.Report != "r" || m.Result == nil || len(m.Result.Rows) != 2 {
		t.Fatalf("%v %s", err, ta.out)
	}
}

func TestReportRunFallaYLimpia(t *testing.T) {
	ta, _, prov := newReportApp(t)
	prov.err = errors.New("model down")
	d := testReportDef(t, "r", "")
	if err := ta.reportRun(context.Background(), d, "", false); err == nil || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.machines) != 0 {
		t.Fatal("a failed run left its copy")
	}
	if !lastRun("r").IsZero() {
		t.Error("a failed run was recorded as good")
	}
}

func TestReportRunCopiaAbandonada(t *testing.T) {
	ta, _, _ := newReportApp(t, "```sql\nSELECT name FROM customers\n```")
	ctx := context.Background()
	d := testReportDef(t, "r", "")
	// Una ejecución anterior murió y dejó su copia: se borra y se sigue.
	old := ta.f.newMachine(reportCopyName("r"), map[string]string{labelGolden: "pg", labelOwner: defaultOwner, labelReport: "r"})
	if err := ta.reportRun(ctx, d, "", false); err != nil {
		t.Fatal(err)
	}
	if ta.f.find(old.ID) != nil || !strings.Contains(ta.err.String(), "left over") {
		t.Fatalf("the stale copy was not removed:\n%s", ta.err)
	}
	// El mismo nombre ocupado por otra cosa no se toca.
	other := ta.f.newMachine(reportCopyName("r"), map[string]string{"x": "y"})
	if err := ta.reportRun(ctx, d, "", false); err == nil || ta.f.find(other.ID) == nil {
		t.Fatalf("err = %v", err)
	}
}

func TestReportRunUnaALaVez(t *testing.T) {
	ta, _, _ := newReportApp(t)
	ctx := context.Background()
	un, err := dbstate.Lock(ctx, "report-r", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer un()
	if err := ta.reportRun(ctx, testReportDef(t, "r", ""), "", false); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.calls) != 0 {
		t.Error("it touched the daemon")
	}
}

func TestReportLs(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	a := testReportDef(t, "a", "")
	b := testReportDef(t, "b", "")
	b.Explain, b.SendData = true, true
	for _, d := range []*reportDef{a, b} {
		if err := addReport(io.Discard, d, false); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	if err := reportLs(&out, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"REPORT", "a ", "b ", "yes (-send-data)", "clientes nuevos"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("ls lacks %q:\n%s", w, out.String())
		}
	}
	out.Reset()
	if err := reportLs(&out, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil || len(rows) != 2 || rows[1]["send_data"] != true || rows[0]["due"] != true {
		t.Fatalf("%v %s", err, out.String())
	}
}

func TestReportUso(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"add", "r"},
		{"add", "r", "-golden", "pg", "-every", "1w"},
		{"run"},
		{"run", "r", "-golden", "pg"},
		{"ls", "-due"},
		{"rm"},
		{"rm", "r", "-json"},
		{"run", "r", "-owner", "x"},
	} {
		if err := cmdReport(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
