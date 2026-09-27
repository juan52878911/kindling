package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestHelpDeComandoDesconocido(t *testing.T) {
	err := cmdHelp([]string{"definitely-not-a-command"})
	if err == nil || hintFor(err) != "kling help all" || codigoDeSalida(err) != 2 {
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
