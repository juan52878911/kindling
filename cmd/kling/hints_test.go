package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestHintFor(t *testing.T) {
	casos := []struct {
		err  error
		want string
	}{
		{errors.New(`Get "http://kling/machines": cannot talk to the daemon at /run/kling.sock: dial unix /run/kling.sock: connect: no such file or directory`), "kling doctor"},
		{errors.New(`cannot talk to the daemon at /run/kling.sock: dial unix /run/kling.sock: connect: permission denied`), "KLING_SOCKET_USER"},
		{errors.New(`machine "web" does not exist`), "kling ps -a"},
		{errors.New(`snapshot "tpl" does not exist`), "kling snapshots ls"},
		{errors.New(`image "foo" does not exist`), "kling images ls"},
		{errors.New(`image "toolchain" does not exist`), "kling images toolchain"},
		{errors.New(`context "lab" does not exist`), "kling context ls"},
		{errors.New(`Post "http://kling/machines": EOF`), "kling doctor"},
		{&api.StatusError{Code: 404, Message: "404 page not found"}, "kling version"},
		{&errWithHint{err: errors.New("x"), hint: "do y"}, "do y"},
		{fmt.Errorf("wrapped: %w", &errWithHint{err: errors.New("x"), hint: "do z"}), "do z"},
		{errors.New("something else entirely"), ""},
		// Un error que ya dice qué hacer no recibe otra pista.
		{errors.New("volume \"v\" not found: create it with `kling volume create v`"), ""},
	}
	for _, c := range casos {
		got := hintFor(c.err)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("hintFor(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	if hintFor(nil) != "" {
		t.Fatal("nil no tiene pista")
	}
}

func TestPrintError(t *testing.T) {
	var b bytes.Buffer
	printError(&b, errors.New(`machine "x" does not exist`))
	if b.String() != "error: machine \"x\" does not exist\ntry: kling ps -a\n" {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	printError(&b, errors.New("no hint here"))
	if b.String() != "error: no hint here\n" {
		t.Fatalf("%q", b.String())
	}
	b.Reset()
	printError(&b, &errConCodigo{code: 1, err: errDoctorQuiet})
	if b.Len() != 0 {
		t.Fatalf("un error vacío no imprime nada: %q", b.String())
	}
}

func TestMovedError(t *testing.T) {
	err := movedError("gateway", "mcp")
	if codigoDeSalida(err) != 2 {
		t.Fatalf("código %d", codigoDeSalida(err))
	}
	if hintFor(err) != "kling plugins install mcp" {
		t.Fatalf("pista %q", hintFor(err))
	}
	if movedToExtension["gateway"] != "mcp" {
		t.Fatal("tabla de comandos movidos")
	}
}
