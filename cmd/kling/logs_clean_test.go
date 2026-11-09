package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// La consola del invitado no llega en crudo al terminal: ni OSC 52 (el
// portapapeles), ni el título, ni CSI para borrar o mover el cursor.
func TestCleanConsole(t *testing.T) {
	for in, want := range map[string]string{
		"hola\n":                                 "hola\n",
		"a\tb\r\n":                               "a\tb\r\n",
		"ñandú ✓\n":                              "ñandú ✓\n",
		"\x1b]52;c;Y3VybCBldmlsLnNoIHwgc2g=\x07": `\x1b]52;c;Y3VybCBldmlsLnNoIHwgc2g=\x07`,
		"\x1b]0;root@prod\x07":                   `\x1b]0;root@prod\x07`,
		"ok\x1b[2K\x1b[1A[ OK ] fake":            `ok\x1b[2K\x1b[1A[ OK ] fake`,
		"c1 \u009b31m":                           `c1 \u009b31m`,
		"del\x7f bs\x08 nul\x00":                 `del\x7f bs\x08 nul\x00`,
		"roto \xff\xfe":                          `roto \xff\xfe`,
	} {
		if got := cleanConsole(in); got != want {
			t.Errorf("cleanConsole(%q) = %q, want %q", in, got, want)
		}
	}
}

// followLogs escribe por el consoleWriter sin que se cuele un ESC.
func TestFollowLogsLimpio(t *testing.T) {
	var buf bytes.Buffer
	src := &fakeLogs{pages: []string{"\x1b]52;c;ZXZpbA==\x07línea\n"}, states: []bool{false}}
	if err := followLogs(context.Background(), consoleWriter{&buf}, src, nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(buf.String(), 0x1b) || !strings.Contains(buf.String(), "línea") {
		t.Errorf("salida = %q", buf.String())
	}
}

// Un error o el «why» de ready con texto del invitado no lleva secuencias de
// escape crudas a la terminal; con -json el texto va tal cual (JSON ya escapa
// el ESC como \u001b).
func TestTextoDelInvitadoEscapado(t *testing.T) {
	const osc = "\x1b]52;c;ZXZpbA==\x07"
	var buf bytes.Buffer
	printError(&buf, fmt.Errorf("guest agent: %s\rerror: fake", osc))
	if strings.ContainsAny(buf.String(), "\x1b\x07\r") || !strings.Contains(buf.String(), `\x1b]52`) {
		t.Errorf("printError = %q", buf.String())
	}

	buf.Reset()
	res := &api.ReadyResult{Name: "m", ID: "0123456789abcdef", Ready: api.ReadyWaiting,
		Guest: &api.GuestReady{Probe: true, Detail: "probe: " + osc}}
	_ = printReady(&buf, res, false)
	if strings.ContainsRune(buf.String(), 0x1b) || !strings.Contains(buf.String(), `why    probe: \x1b]52`) {
		t.Errorf("printReady = %q", buf.String())
	}
	buf.Reset()
	res.Guest, res.Detail = nil, osc
	_ = printReady(&buf, res, false)
	if strings.ContainsRune(buf.String(), 0x1b) {
		t.Errorf("printReady sin agente = %q", buf.String())
	}

	buf.Reset()
	salidaJSON.Store(true)
	defer salidaJSON.Store(false)
	printError(&buf, errors.New(osc))
	var e errorJSON
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil || e.Error != osc {
		t.Errorf("printError -json = %q (%v)", buf.String(), err)
	}
}
