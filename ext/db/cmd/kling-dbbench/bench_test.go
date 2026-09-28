package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- stats ----

func TestPercentileRangoMasCercano(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for p, want := range map[float64]float64{0: 1, 50: 5, 90: 9, 95: 10, 99: 10, 100: 10} {
		if got := percentile(xs, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("serie vacía")
	}
}

func TestSummarizeNoModificaLaEntrada(t *testing.T) {
	xs := []float64{30, 10, 20}
	d := summarize(xs)
	if xs[0] != 30 || d.N != 3 || d.P50 != 20 || d.Max != 30 || d.Mean != 20 {
		t.Fatalf("d = %+v xs = %v", d, xs)
	}
	if summarize(nil) != (Dist{}) {
		t.Error("vacío")
	}
}

func TestFailStatus(t *testing.T) {
	for _, c := range []struct {
		f, n int
		want string
	}{{0, 100, "ok"}, {1, 100, "ok"}, {2, 100, "DEGRADED"}, {1, 8, "DEGRADED"}, {0, 1, "ok"}, {1, 1, "DEGRADED"}} {
		if got := failStatus(c.f, c.n); got != c.want {
			t.Errorf("%d/%d = %s, want %s", c.f, c.n, got, c.want)
		}
	}
}

// ---- parsers ----

func TestParseRunOutput(t *testing.T) {
	r, err := parseRunOutput("0123456789ab  dbb-1  instantiated from golden-pg in 37 ms\n  next: kling logs\n")
	if err != nil || r.ID != "0123456789ab" || r.Name != "dbb-1" || r.From != "golden-pg" || r.Ms != 37 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = parseRunOutput("0123456789ab  x  booted cold in 2600 ms")
	if err != nil || r.From != "" || r.Ms != 2600 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := parseRunOutput("error: nope"); err == nil {
		t.Fatal("salida rara aceptada")
	}
}

func TestParseInspect(t *testing.T) {
	a, err := parseInspect([]byte(`{"id":"x","ip":"10.0.0.7"}`), 5432)
	if err != nil || a != "10.0.0.7:5432" {
		t.Fatalf("%q %v", a, err)
	}
	// macOS: reenvío por loopback.
	a, err = parseInspect([]byte(`{"id":"x","ip":"10.0.2.15","forwards":{"5432":"127.0.0.1:61234"}}`), 5432)
	if err != nil || a != "127.0.0.1:61234" {
		t.Fatalf("%q %v", a, err)
	}
	if _, err := parseInspect([]byte(`{"id":"x"}`), 5432); err == nil {
		t.Fatal("sin dirección debía fallar")
	}
	if _, err := parseInspect([]byte(`no json`), 5432); err == nil {
		t.Fatal("no json")
	}
}

func TestParseFork(t *testing.T) {
	r, err := parseFork([]byte(`{"snapshot":"fork-1","sandboxes":[{"id":"a","ip":"10.0.0.2"},{"id":"b","ip":"10.0.0.3"}]}`))
	if err != nil || r.Snapshot != "fork-1" || len(r.Sandboxes) != 2 || r.Sandboxes[1].Addr(5432) != "10.0.0.3:5432" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestParseDockerPort(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:49153\n":         "127.0.0.1:49153",
		"0.0.0.0:32768\n[::]:32768": "127.0.0.1:32768",
		"[::]:1234":                 "127.0.0.1:1234",
	} {
		if got, err := parseDockerPort(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	if _, err := parseDockerPort("Error: No public port"); err == nil {
		t.Error("error aceptado")
	}
}

func TestParseProc(t *testing.T) {
	mi := "MemTotal:       16384000 kB\nMemFree:  100 kB\nMemAvailable:    8192000 kB\n"
	if v, ok := parseMemAvailableMiB(mi); !ok || v != 8000 {
		t.Errorf("avail = %d %v", v, ok)
	}
	if v, ok := parseMemTotalMiB(mi); !ok || v != 16000 {
		t.Errorf("total = %d %v", v, ok)
	}
	if _, ok := parseMemAvailableMiB("nada"); ok {
		t.Error("sin clave")
	}
	psi := "some avg10=1.25 avg60=0.50 avg300=0.10 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	if v, ok := parsePSISome(psi); !ok || v != 1.25 {
		t.Errorf("psi = %v %v", v, ok)
	}
	if _, ok := parsePSISome(""); ok {
		t.Error("psi vacío")
	}
}

// ---- reglas de saltar celdas ----

func TestSkipReason(t *testing.T) {
	if skipReason(8, 256, 128, 10000, 100000) != "" {
		t.Error("8×256 cabe en 10000")
	}
	if r := skipReason(32, 256, 128, 10000, 100000); !strings.Contains(r, "MemAvailable") || !strings.Contains(r, "8192") {
		t.Errorf("RAM: %q", r)
	}
	if r := skipReason(8, 0, 1000, 10000, 4000); !strings.Contains(r, "free disk") {
		t.Errorf("disco: %q", r)
	}
	if skipReason(64, 256, 1000, -1, -1) != "" {
		t.Error("sin medida no se salta")
	}
}

// ---- agregación ----

func TestBuildCellsPoolaYMarcaDegraded(t *testing.T) {
	r1 := Round{Mode: "docker", N: 2, Rep: 1, OK: 2, MemAvail: MemStats{Before: 1000, Min: 900, PeakUsed: 100}, DiskPerCopyMiB: 10,
		Copies: []CopyResult{{OK: true, MS: 100}, {OK: true, MS: 300}}}
	r2 := Round{Mode: "docker", N: 2, Rep: 2, OK: 1, Failed: 1, Errors: map[string]int{"connect:timeout": 1}, MemAvail: MemStats{Before: 1000, Min: 800, PeakUsed: 200}, DiskPerCopyMiB: -1,
		Copies: []CopyResult{{OK: true, MS: 200}, {Phase: "connect:timeout"}}}
	skip := Round{Mode: "docker", N: 32, Status: "skipped", Reason: "no cabe"}
	cells := buildCells([]Round{r1, r2, skip})
	if len(cells) != 2 {
		t.Fatalf("cells = %+v", cells)
	}
	c := cells[0]
	if c.Copies != 4 || c.OK != 3 || c.Failed != 1 || c.Status != "DEGRADED" || c.Latency.Max != 300 || c.Latency.N != 3 ||
		c.MemAvailMinMiB != 800 || c.MemPeakUsedMiB != 200 || c.DiskPerCopyMiB != 10 || c.Errors["connect:timeout"] != 1 {
		t.Fatalf("c = %+v", c)
	}
	if cells[1].Status != "skipped" || cells[1].Reason != "no cabe" {
		t.Fatalf("skip = %+v", cells[1])
	}
	var sb strings.Builder
	writeMarkdown(&sb, &Report{Rounds: []Round{r1, r2, skip}, Cells: cells, Config: Public{Modes: []string{"docker"}}})
	for _, want := range []string{"| docker | 2 | DEGRADED | 3/4 |", "| docker | 32 | skipped |", "connect:timeout×1", "Every round"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("falta %q en\n%s", want, sb.String())
		}
	}
}

func TestDiskPerCopy(t *testing.T) {
	rs := []CopyResult{{OK: true}, {OK: true}, {}}
	if got := diskPerCopy(20<<20, rs); got != 10 {
		t.Errorf("got %v", got)
	}
	if diskPerCopy(-1, rs) != -1 || diskPerCopy(5, []CopyResult{{}}) != -1 {
		t.Error("delta negativo o sin copias debe ser n/d")
	}
}

// ---- rondas contra un Postgres falso y herramientas falsas ----

// pgFalso contesta a todo con un count fijo (autenticación trust).
func pgFalso(t *testing.T, count string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("sin loopback:", err)
	}
	t.Cleanup(func() { ln.Close() })
	msg := func(tp byte, body []byte) []byte {
		b := binary.BigEndian.AppendUint32([]byte{tp}, uint32(4+len(body)))
		return append(b, body...)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				var l [4]byte
				if _, err := io.ReadFull(r, l[:]); err != nil {
					return
				}
				io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(l[:]))-4)
				c.Write(msg('R', []byte{0, 0, 0, 0}))
				c.Write(msg('Z', []byte("I")))
				for {
					var h [5]byte
					if _, err := io.ReadFull(r, h[:]); err != nil {
						return
					}
					io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(h[1:]))-4)
					if h[0] == 'X' {
						return
					}
					row := binary.BigEndian.AppendUint16(nil, 1)
					row = binary.BigEndian.AppendUint32(row, uint32(len(count)))
					c.Write(msg('D', append(row, count...)))
					c.Write(msg('C', []byte("SELECT 1\x00")))
					c.Write(msg('Z', []byte("I")))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func testConfig() *config {
	return &config{Table: "items", Expect: 100, User: "u", DB: "d", Timeout: 5 * time.Second, PGPort: 5432,
		Kling: "kling", Golden: "golden", Prefix: "dbb", RunID: "t1", PSIMax: 5, Pause: 0}
}

// klingFalso simula `kling run` y `kling inspect` y apunta las copias a addr.
type klingFalso struct {
	mu    sync.Mutex
	addr  string
	calls []string
	rm    []string
}

func (k *klingFalso) run(_ context.Context, _ []string, name string, args ...string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, strings.Join(args, " "))
	switch args[0] {
	case "run":
		return "0123456789ab  " + args[4] + "  instantiated from golden in 12 ms\n", nil
	case "inspect":
		return fmt.Sprintf(`{"id":"0123456789ab","forwards":{"5432":%q}}`, k.addr), nil
	case "sandbox":
		return fmt.Sprintf(`{"snapshot":"s","sandboxes":[{"id":"a","forwards":{"5432":%q}},{"id":"b","forwards":{"5432":%q}}]}`, k.addr, k.addr), nil
	case "rm":
		k.rm = append(k.rm, args[2:]...)
		return "", nil
	}
	return "", fmt.Errorf("unexpected %v", args)
}

func TestRondaKindlingRun(t *testing.T) {
	cfg := testConfig()
	k := &klingFalso{addr: pgFalso(t, "100")}
	b := &klingBackend{cfg: cfg, run: k.run}
	if err := b.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := runRound(context.Background(), b, cfg, 4, 1)
	if r.Status != "ok" || r.OK != 4 || r.Failed != 0 || r.Latency.N != 4 || r.Copies[0].ToolMS != 12 {
		t.Fatalf("r = %+v", r)
	}
	if len(k.rm) != 4 {
		t.Fatalf("cleanup borró %v", k.rm)
	}
}

func TestRondaKindlingForkYRecuentoErroneo(t *testing.T) {
	cfg := testConfig()
	cfg.ForkSrc = "src"
	k := &klingFalso{addr: pgFalso(t, "99")} // el seed no está completo
	b := &klingBackend{cfg: cfg, run: k.run, fork: true}
	r := runRound(context.Background(), b, cfg, 3, 1) // el falso devuelve 2 copias: la 3ª falla
	if r.Status != "DEGRADED" || r.Failed != 3 || r.Errors["wrong_count"] != 2 || r.Errors["provision"] != 1 {
		t.Fatalf("r = %+v", r)
	}
	if len(k.rm) != 2 {
		t.Fatalf("cleanup borró %v", k.rm)
	}
}

func TestFirstCountTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	// Puerto cerrado: se reintenta hasta agotar el plazo y se clasifica.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	phase, err := firstCount(ctx, target{Addr: addr, User: "u", Database: "d"}, cfg)
	if err == nil || phase != "connect" {
		t.Fatalf("phase=%q err=%v", phase, err)
	}
}

func TestRondaDocker(t *testing.T) {
	cfg := testConfig()
	cfg.Docker, cfg.DockerImage, cfg.seedText = "docker", "postgres:16-alpine", "CREATE TABLE items(id int);"
	addr := pgFalso(t, "100")
	var mu sync.Mutex
	var calls []string
	var envs []string
	run := func(_ context.Context, env []string, name string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		envs = append(envs, env...)
		switch args[0] {
		case "port":
			return addr + "\n", nil
		case "ps":
			return "c1\nc2\n", nil
		}
		return "", nil
	}
	b := &dockerBackend{cfg: cfg, run: run}
	if err := b.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := runRound(context.Background(), b, cfg, 2, 1)
	if r.Status != "ok" || r.OK != 2 {
		t.Fatalf("r = %+v", r)
	}
	all := strings.Join(calls, "\n")
	if !strings.Contains(all, "-p 127.0.0.1::5432") || !strings.Contains(all, "rm -f -v c1 c2") {
		t.Errorf("órdenes inesperadas:\n%s", all)
	}
	if strings.Contains(all, b.pw) {
		t.Error("la contraseña no debe ir en argv")
	}
	if len(envs) == 0 || !strings.HasPrefix(envs[0], "POSTGRES_PASSWORD=") {
		t.Errorf("env = %v", envs)
	}
}
