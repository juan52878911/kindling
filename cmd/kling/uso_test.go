package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// envComoKling hace que el binario de test corra main (TestMain).
const envComoKling = "KLING_TEST_AS_KLING"

// correrKling corre `kling args...` en un proceso aparte con el main de
// verdad y devuelve su stdout, su stderr y su código de salida. KLING_HOST
// apunta a un socket que no existe: un comando que llegue a hablar con el
// daemon falla con 1.
func correrKling(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), envComoKling+"=1", "KLING_HOST=unix:///nonexistent/kling.sock")
	cmd.Env = append(cmd.Env, env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Fatalf("kling %v: %v", args, err)
	}
	return o.String(), e.String(), code
}

// Un flag que no existe es un error de uso: 2, "error:" y la ayuda del
// comando como pista, sin el Usage crudo del paquete flag.
func TestFlagDesconocidoEsErrorDeUso(t *testing.T) {
	hermetic(t)
	for _, c := range []struct {
		args []string
		try  string
	}{
		{[]string{"ps", "-x"}, "try: kling help ps"},
		{[]string{"machine", "squeeze", "-nope", "m"}, "try: kling help machine squeeze"},
		{[]string{"image", "ls", "-bogus"}, "try: kling help image ls"},
	} {
		out, errOut, code := correrKling(t, nil, c.args...)
		if code != salidaUso {
			t.Errorf("kling %v: código %d, quería %d\n%s", c.args, code, salidaUso, errOut)
		}
		if !strings.HasPrefix(errOut, "error: flag provided but not defined") || !strings.Contains(errOut, c.try) {
			t.Errorf("kling %v: stderr\n%s", c.args, errOut)
		}
		if strings.Contains(errOut, "Usage of") || out != "" {
			t.Errorf("kling %v: Usage crudo o stdout no vacío:\n%s%s", c.args, out, errOut)
		}
	}
}

// -h sigue dando la lista de flags y sale con 0: es lo que plugin.FlagHelp
// le pide a cada comando para `kling help`.
func TestFlagAyudaListaFlags(t *testing.T) {
	hermetic(t)
	_, errOut, code := correrKling(t, []string{"KLING_FLAG_HELP=1"}, "ps", "-h")
	if code != 0 || !strings.Contains(errOut, "  -json") || strings.Contains(errOut, "error:") {
		t.Fatalf("ps -h: código %d\n%s", code, errOut)
	}
}

// Un subcomando que no existe sale con 2 en todos los grupos, no con 1.
func TestSubcomandoDesconocidoSaleConDos(t *testing.T) {
	hermetic(t)
	for nombre, f := range map[string]func([]string) error{
		"machine": cmdMachine, "image": cmdImages, "volume": cmdVolume, "graph": cmdGraph,
		"sandbox": cmdSandbox, "template": cmdTemplate, "registry": cmdRegistry, "cow": cmdCoW,
		"ai": cmdAI, "compose": cmdCompose,
	} {
		err := f([]string{"nope"})
		if err == nil || codigoDeSalida(err) != salidaUso {
			t.Errorf("%s nope: %v, código %d", nombre, err, codigoDeSalida(err))
			continue
		}
		if h := hintFor(err); !strings.HasPrefix(h, "kling help") && nombre != "compose" {
			t.Errorf("%s nope: pista %q", nombre, h)
		}
	}
	if err := cmdImages([]string{"snapshots", "nope"}); codigoDeSalida(err) != salidaUso {
		t.Errorf("image snapshots nope: %v", err)
	}
}

// Los comandos sin argumentos posicionales no se tragan uno de más.
func TestArgumentosSobrantes(t *testing.T) {
	hermetic(t)
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	for nombre, f := range map[string]func([]string) error{
		"ps": cmdPS, "top": cmdTop, "events": cmdEvents, "topo": cmdTopo,
		"version": cmdVersion, "status": cmdStatus, "doctor": cmdDoctor,
	} {
		err := f([]string{"sobra"})
		if codigoDeSalida(err) != salidaUso || hintFor(err) != "kling help "+nombre {
			t.Errorf("%s sobra: %v (código %d, pista %q)", nombre, err, codigoDeSalida(err), hintFor(err))
		}
	}
}

func TestComandoDeAyuda(t *testing.T) {
	for msg, quiero := range map[string]string{
		"usage: kling machine squeeze <ref>...":             "machine squeeze",
		"usage: kling save -audit <ref>":                    "save",
		"usage: kling image snapshots rm <name>":            "image snapshots",
		"usage: kling compose exec [-T] <service> <cmd>":    "compose exec",
		"missing machine":                                   "",
		"usage: kling machine <resize|squeeze> <ref> [...]": "machine",
	} {
		if got := comandoDeAyuda(comandoDeUsage(msg)); got != quiero {
			t.Errorf("%q: %q, quería %q", msg, got, quiero)
		}
	}
	for fs, quiero := range map[string]string{
		"squeeze": "machine squeeze", "mmds": "machine secret", "credential": "machine credential",
		"plugins install": "plugin install", "images cat": "image cat", "models ls": "ai model",
		"nombre": "",
	} {
		if got := comandoDeAyuda(fs); got != quiero {
			t.Errorf("FlagSet %q: %q, quería %q", fs, got, quiero)
		}
	}
	// El error de parseFlags lleva el nombre del FlagSet.
	f := flag.NewFlagSet("ps", flag.ContinueOnError)
	err := parseFlags(f, []string{"-x"})
	if hintFor(err) != "kling help ps" || codigoDeSalida(err) != salidaUso {
		t.Errorf("parseFlags: %v %q", err, hintFor(err))
	}
}
