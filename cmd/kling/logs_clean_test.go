package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
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
