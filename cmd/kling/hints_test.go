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
		{errors.New(`snapshot "tpl" does not exist`), "kling template ls"},
		{errors.New(`image "foo" does not exist`), "kling image ls"},
		{errors.New(`image "toolchain" does not exist`), "kling image toolchain"},
		{errors.New(`context "lab" does not exist`), "kling context ls"},
		{errors.New(`Post "http://kling/machines": EOF`), "kling doctor"},
		{&api.StatusError{Code: 404, Message: "404 page not found"}, "kling version"},
		{&errWithHint{err: errors.New("x"), hint: "do y"}, "do y"},
		{fmt.Errorf("wrapped: %w", &errWithHint{err: errors.New("x"), hint: "do z"}), "do z"},
		{&api.StatusError{Code: 429, Message: "guest agent: too many commands running in this microVM (limit 64)", ErrorCode: api.CodeGuestBusy}, "retry when some"},
		{&api.StatusError{Code: 429, Message: "tenant a: quota exceeded", ErrorCode: api.CodeTenantQuota}, ""},
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
	if hintFor(err) != "kling plugin install mcp" {
		t.Fatalf("pista %q", hintFor(err))
	}
	if movedToExtension["mcp"] != "mcp" || movedToExtension["connect"] != "mcp" {
		t.Fatal("tabla de comandos movidos")
	}
}

// Los códigos estables del daemon deciden la pista antes que el texto: un
// disco o una memoria que no alcanzan no tenían ninguna.
func TestPistasPorCodigo(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&api.StatusError{Code: 503, Message: "only 900 MiB of disk left under /var/lib/kindling", ErrorCode: api.CodeDiskFull}, "kling template ls"},
		{&api.StatusError{Code: 507, Message: "not enough memory for 2048 MiB", ErrorCode: api.CodeInsufficientMemory}, "kling freeze"},
		// El código manda sobre una regla de texto que también casaría.
		{&api.StatusError{Code: 503, Message: `image "x" does not exist`, ErrorCode: api.CodeDiskFull}, "kling template ls"},
		// Un mensaje que ya dice qué teclear no recibe otra.
		{&api.StatusError{Code: 507, Message: "under memory pressure. Freeze idle instances (`kling ps`)", ErrorCode: api.CodeInsufficientMemory}, ""},
		{&api.StatusError{Code: 500, Message: "missing kernel at /var/lib/kindling/images/vmlinux"}, "kling up -check"},
		{&api.StatusError{Code: 400, Message: `invalid image name "a b"`}, "letters, digits"},
		{&api.StatusError{Code: 400, Message: `invalid name "-x"`}, "letters, digits"},
		{&api.StatusError{Code: 400, Message: `invalid volume name "V"`}, "lowercase letters"},
		{&api.StatusError{Code: 400, Message: `invalid volume name "V": lowercase letters, digits, hyphen and underscore`}, ""},
	} {
		got := hintFor(c.err)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("hintFor(%q) = %q, want %q", c.err, got, c.want)
		}
	}
}

// `kling db` sin la extensión dice cuál instalar.
func TestDBMovidoAExtension(t *testing.T) {
	if movedToExtension["db"] != "db" {
		t.Fatal("db no está en movedToExtension")
	}
	if h := hintFor(movedError("db", "db")); h != "kling plugin install db" {
		t.Fatalf("pista %q", h)
	}
}

// `kling save` sin daemon dice que no hay daemon, no que el invitado no
// sirve: el error de transporte sale tal cual, con su pista.
func TestSaveSinDaemon(t *testing.T) {
	hermetic(t)
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	var err error
	salida, _ := capturarStdout(t, func() error { err = cmdSave([]string{"m1", "tpl"}); return err })
	if err == nil || strings.Contains(err.Error(), "not serving") {
		t.Fatalf("save sin daemon: %v", err)
	}
	if h := hintFor(err); h != "kling doctor" {
		t.Fatalf("pista %q (error %v)", h, err)
	}
	_ = salida
}
