package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestUsageBlock(t *testing.T) {
	text := usageHead + usageTail
	run := usageBlock("run", text)
	if !strings.HasPrefix(run, "  run [-name N]") || !strings.Contains(run, "[-share SRC:DST") {
		t.Fatalf("el bloque de run ha de llevar sus continuaciones:\n%s", run)
	}
	if !strings.Contains(run, "  run -from <name>") {
		t.Fatalf("run sale también en GOLDEN SNAPSHOTS:\n%s", run)
	}
	if strings.Contains(run, "ps [-a]") || strings.Contains(run, "MACHINES") {
		t.Fatalf("solo el bloque del comando:\n%s", run)
	}
	for _, cmd := range []string{"doctor", "try", "logs", "snapshots", "rmi", "version", "completion", "context", "config", "sandbox", "help", "exec"} {
		if usageBlock(cmd, text) == "" {
			t.Errorf("%s no sale en la ayuda", cmd)
		}
	}
	if !strings.Contains(usageBlock("logs", text), "-f") {
		t.Error("logs -f no está en la ayuda")
	}
	if usageBlock("nope", text) != "" {
		t.Error("un comando que no existe no tiene bloque")
	}
	// Una línea de continuación no se confunde con un comando.
	if usageBlock("[-egress", text) != "" {
		t.Error("solo las líneas con dos espacios abren bloque")
	}
}

func TestHelpDeComandoDesconocido(t *testing.T) {
	err := cmdHelp("definitely-not-a-command")
	if err == nil || hintFor(err) != "kling help" || codigoDeSalida(err) != 2 {
		t.Fatalf("%v / %q", err, hintFor(err))
	}
}

func TestWriteVersion(t *testing.T) {
	var b bytes.Buffer
	_ = writeVersion(&b, "v0.13.0", "/run/kling.sock", &api.Info{Version: "0.13.0"}, nil, false)
	if b.String() != "kling v0.13.0\ndaemon: 0.13.0 (/run/kling.sock)\n" {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "/run/kling.sock", &api.Info{Version: "0.12.0"}, nil, false)
	if !strings.Contains(b.String(), "note: CLI and daemon differ") {
		t.Fatalf("desfase sin avisar: %q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "ssh://x", nil, errors.New("down"), false)
	if !strings.HasPrefix(b.String(), "kling v0.13.0\n") || !strings.Contains(b.String(), "not reachable at ssh://x") {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	_ = writeVersion(&b, "v0.13.0", "s", &api.Info{Version: "0.13.0"}, nil, true)
	var v map[string]string
	if err := json.Unmarshal(b.Bytes(), &v); err != nil || v["cli"] != "v0.13.0" || v["daemon"] != "0.13.0" || v["endpoint"] != "s" {
		t.Fatalf("%v %v", v, err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	var l limitedBuffer
	l.max = 4
	n, err := l.Write([]byte("abcdef"))
	if n != 6 || err != nil || l.String() != "abcd" {
		t.Fatalf("%d %v %q", n, err, l.String())
	}
}
