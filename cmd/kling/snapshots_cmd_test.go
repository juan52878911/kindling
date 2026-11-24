package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

func TestWriteSnapshots(t *testing.T) {
	var b bytes.Buffer
	if err := writeSnapshots(&b, nil, true); err != nil || strings.TrimSpace(b.String()) != "[]" {
		t.Fatalf("una lista vacía en JSON es []: %q", b.String())
	}
	b.Reset()
	list := []*api.Snapshot{{Name: "tpl", Image: "toolchain", VCPUs: 1, MemMiB: 256, MemBytes: 40 << 20, CreatedAt: time.Now()}}
	if err := writeSnapshots(&b, list, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "NAME") || !strings.Contains(b.String(), "tpl") || !strings.Contains(b.String(), "40M") {
		t.Fatalf("%s", b.String())
	}
	// F2: sin GuestIPv6Off (el caso de arriba, campo en su cero) se marca en
	// la tabla y se explica al pie.
	if !strings.Contains(b.String(), "tpl*") || !strings.Contains(b.String(), "IPv6 barrier") {
		t.Errorf("faltó la marca de F2 (sin GuestIPv6Off):\n%s", b.String())
	}

	b.Reset()
	list[0].GuestIPv6Off = true
	if err := writeSnapshots(&b, list, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "*") {
		t.Errorf("con GuestIPv6Off no debería haber marca ni pie:\n%s", b.String())
	}

	// Un dorado de una imagen que pide la pila IPv6 (Android) no es "viejo".
	b.Reset()
	list[0].GuestIPv6Off, list[0].GuestIPv6Stack = false, true
	if err := writeSnapshots(&b, list, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "*") {
		t.Errorf("con GuestIPv6Stack no debería haber marca ni pie:\n%s", b.String())
	}
}

func TestWriteSnapshot(t *testing.T) {
	s := &api.Snapshot{Name: "tpl", Image: "toolchain", VCPUs: 2, MemMiB: 512, MemMaxMiB: 1024,
		CreatedAt: time.Now().Add(-time.Hour), Instances: 3,
		Volumes:     []api.VolumeAttachment{{Name: "data", Mount: "/data", ReadOnly: true}},
		Annotations: map[string]json.RawMessage{"mcp.catalog": json.RawMessage(`{"tools": ["a", "b"]}`), "b": json.RawMessage(`1`)}}
	var b bytes.Buffer
	if err := writeSnapshot(&b, s, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"name:        tpl", "2 / 512 MiB (ceiling 1024 MiB)", "instances:   3",
		"data -> /data (ro)", "annotations:", `mcp.catalog  {"tools": ["a", "b"]}`, "kling run -from tpl",
		"guest ipv6:  not confirmed off"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en:\n%s", want, out)
		}
	}
	if strings.Index(out, "  b  1") > strings.Index(out, "mcp.catalog") {
		t.Error("las anotaciones van ordenadas")
	}

	// Con GuestIPv6Off no se muestra el aviso.
	b.Reset()
	s2 := *s
	s2.GuestIPv6Off = true
	if err := writeSnapshot(&b, &s2, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "guest ipv6") {
		t.Errorf("con GuestIPv6Off no debería mencionar el ipv6 del invitado:\n%s", b.String())
	}
	b.Reset()
	s3 := *s
	s3.GuestIPv6Stack = true
	if err := writeSnapshot(&b, &s3, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "stack kept by the image") || strings.Contains(b.String(), "not confirmed off") {
		t.Errorf("con GuestIPv6Stack se dice que lo pidió la imagen:\n%s", b.String())
	}

	b.Reset()
	if err := writeSnapshot(&b, s, true); err != nil {
		t.Fatal(err)
	}
	var back api.Snapshot
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Name != "tpl" || len(back.Annotations) != 2 {
		t.Fatalf("%v %+v", err, back)
	}
}

// Un dorado obsoleto (hecho con otro VMM) se marca en la tabla y en inspect
// con la causa y la orden para rehacerlo; con qué se hizo se enseña siempre.
func TestSnapshotObsoletoSeVe(t *testing.T) {
	s := &api.Snapshot{Name: "tpl", Image: "toolchain", VCPUs: 1, MemMiB: 256, CreatedAt: time.Now(), GuestIPv6Off: true,
		VMM: "firecracker 1.12.0", KlingVersion: "0.18.0",
		Stale: "made with firecracker 1.12.0; this host runs firecracker 1.17.0"}
	var b bytes.Buffer
	if err := writeSnapshots(&b, []*api.Snapshot{s}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "tpl!") || !strings.Contains(b.String(), "! stale") || strings.Contains(b.String(), "IPv6") {
		t.Errorf("falta la marca de obsoleto:\n%s", b.String())
	}
	b.Reset()
	if err := writeSnapshot(&b, s, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"made with:   firecracker 1.12.0, kling 0.18.0",
		"stale:       made with firecracker 1.12.0; this host runs firecracker 1.17.0", "kling save -replace <machine> tpl"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("falta %q en:\n%s", want, b.String())
		}
	}
	b.Reset()
	s.Stale = ""
	if err := writeSnapshots(&b, []*api.Snapshot{s}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "!") {
		t.Errorf("sin Stale no hay marca:\n%s", b.String())
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt(json.RawMessage("{\n  \"a\":   1\n}"), 60); got != `{ "a": 1 }` {
		t.Fatalf("%q", got)
	}
	long := json.RawMessage(`"` + strings.Repeat("ñ", 100) + `"`)
	got := excerpt(long, 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("recorta por runas: %q", got)
	}
}

func TestSnapshotsInspectContraUnDaemonFalso(t *testing.T) {
	d := fakeSnapshotsMux()
	c := fakeDaemon(t, d)
	s, err := c.Snapshot(t.Context(), "tpl")
	if err != nil || s.Name != "tpl" {
		t.Fatalf("%v %+v", err, s)
	}
	_, err = c.Snapshot(t.Context(), "nope")
	if err == nil || hintFor(err) != "kling template ls" {
		t.Fatalf("%v / %q", err, hintFor(err))
	}
}

// -upstream en una credencial HTTP (template credential y machine credential
// comparten las banderas): exige -allow-request, admite -ca-file y
// -tls-server-name solo con él, y disable solo hacia el loopback. La salida
// dice a dónde va la clave.
func TestFlagUpstreamHTTP(t *testing.T) {
	dir := t.TempDir()
	clave := filepath.Join(dir, "clave")
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(clave, []byte("tok-inquilino\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := func(args ...string) (api.CredentialSpec, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cf := credentialFlags(fs)
		base := []string{"-f", clave, "-domain", "l3.kindling.internal", "-env", "L3_TOKEN"}
		if err := fs.Parse(append(base, args...)); err != nil {
			return api.CredentialSpec{}, err
		}
		return cf.spec()
	}
	const regla = "POST /v1/chat/completions"

	s, err := spec("-upstream", "127.0.0.1:8080", "-upstream-tls", "disable", "-allow-request", regla)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "" || s.Upstream != "127.0.0.1:8080" || s.UpstreamTLS != credproxy.UpstreamTLSDisable ||
		len(s.Allow) != 1 || s.Secret != "tok-inquilino" {
		t.Fatalf("spec %+v", s)
	}
	if got := describirSitios(s); !strings.Contains(got, "127.0.0.1:8080") || !strings.Contains(got, "unencrypted") {
		t.Errorf("describirSitios: %q", got)
	}
	s, err = spec("-upstream", "10.0.0.5:8443", "-tls-server-name", "gw.vpc.lan", "-ca-file", ca, "-allow-request", regla)
	if err != nil {
		t.Fatal(err)
	}
	if s.Upstream != "10.0.0.5:8443" || s.TLSServerName != "gw.vpc.lan" || !strings.HasPrefix(s.CAPEM, "-----BEGIN") || s.UpstreamTLS != "" {
		t.Fatalf("spec %+v", s)
	}
	if got := describirSitios(s); !strings.Contains(got, "10.0.0.5:8443") || !strings.Contains(got, "gw.vpc.lan") {
		t.Errorf("describirSitios: %q", got)
	}
	// Sin upstream, la salida de siempre.
	if s, err := spec(); err != nil || strings.Contains(describirSitios(s), "upstream") {
		t.Fatalf("sin upstream: %q, %v", describirSitios(s), err)
	}

	for nombre, args := range map[string][]string{
		"sin allow":             {"-upstream", "127.0.0.1:8080"},
		"disable fuera":         {"-upstream", "10.0.0.5:8080", "-upstream-tls", "disable", "-allow-request", regla},
		"disable con nombre":    {"-upstream", "gw.lan:8080", "-upstream-tls", "disable", "-allow-request", regla},
		"disable sin upstream":  {"-upstream-tls", "disable", "-allow-request", regla},
		"disable con CA":        {"-upstream", "127.0.0.1:8080", "-upstream-tls", "disable", "-ca-file", ca, "-allow-request", regla},
		"CA sin upstream":       {"-ca-file", ca, "-allow-request", regla},
		"nombre sin upstream":   {"-tls-server-name", "gw.lan", "-allow-request", regla},
		"tls raro":              {"-upstream", "127.0.0.1:8080", "-upstream-tls", "require", "-allow-request", regla},
		"upstream con rol":      {"-upstream", "127.0.0.1:8080", "-user", "app", "-allow-request", regla},
		"upstream con un sigv4": {"-type", "sigv4", "-access-key-id", "AKIAX", "-region", "auto", "-upstream", "127.0.0.1:8080"},
	} {
		if _, err := spec(args...); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
}
