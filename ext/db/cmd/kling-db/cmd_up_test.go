package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Sin la plantilla por defecto, up la construye una vez desde la imagen por
// defecto y después hace la copia; el ruido del script no sale si va bien.
func TestUpSinPlantillaLaConstruye(t *testing.T) {
	ta := newTestApp(t)
	delete(ta.f.snaps, defaultTemplate)
	var built [][]string
	ta.runGolden = func(_ context.Context, _ string, args []string, w io.Writer) error {
		built = append(built, args)
		io.WriteString(w, "db-golden: lots of steps\n")
		ta.f.snaps[defaultTemplate] = &api.Snapshot{Name: defaultTemplate, Labels: map[string]string{}}
		return nil
	}
	if err := ta.ensureTemplate(ctx, "", defaultTemplate, defaultTemplateOCI); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || strings.Join(built[0], " ") != "build -image "+nombreImagenOCI(defaultTemplateOCI)+" -loopback pg" {
		t.Fatalf("builds %v", built)
	}
	if len(ta.f.imports) != 1 || ta.f.imports[0] != defaultTemplateOCI {
		t.Fatalf("imports %v", ta.f.imports)
	}
	if strings.Contains(ta.err.String(), "lots of steps") {
		t.Fatalf("the script output was shown although it worked: %s", ta.err)
	}
	// Ya existe: ni se importa ni se construye otra vez.
	if err := ta.ensureTemplate(ctx, "", defaultTemplate, defaultTemplateOCI); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || len(ta.f.imports) != 1 {
		t.Fatalf("built again: %v %v", built, ta.f.imports)
	}
}

func TestUpConstruccionFallidaEnseñaElScript(t *testing.T) {
	ta := newTestApp(t)
	delete(ta.f.snaps, defaultTemplate)
	ta.runGolden = func(_ context.Context, _ string, _ []string, w io.Writer) error {
		io.WriteString(w, "db-golden: initdb failed\n")
		return errors.New("exit status 1")
	}
	err := ta.ensureTemplate(ctx, "", defaultTemplate, defaultTemplateOCI)
	if err == nil || !strings.Contains(err.Error(), "kling db golden -from-oci") {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(ta.err.String(), "initdb failed") {
		t.Fatalf("the script output was hidden: %q", ta.err)
	}
}

// Una plantilla con nombre que no existe no se construye sin -from-oci: el
// error dice cómo.
func TestUpPlantillaNombradaQueFalta(t *testing.T) {
	ta := newTestApp(t)
	ta.runGolden = func(context.Context, string, []string, io.Writer) error {
		t.Fatal("built a template nobody asked for")
		return nil
	}
	err := ta.ensureTemplate(ctx, "", "crm", "")
	if err == nil || !strings.Contains(err.Error(), "kling db up crm -from-oci") {
		t.Fatalf("error %v", err)
	}
}

// Otro fallo del daemon no se toma por «no existe».
func TestUpOtroFalloNoConstruye(t *testing.T) {
	if errNoTemplate(errors.New("dial unix: connection refused")) {
		t.Fatal("a daemon error was taken for a missing template")
	}
}

func TestUpJSONSinClaveYConDSN(t *testing.T) {
	ta := newTestApp(t)
	mc, err := ta.up(ctx, "pg", "c1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	mc.IP = "172.16.0.2"
	pw := password(t, mc.ID)
	if err := ta.printUp(ctx, mc, true, false); err != nil {
		t.Fatal(err)
	}
	var got upJSON
	if err := json.Unmarshal(ta.out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, ta.out)
	}
	if got.Name != "c1" || got.Port != 5432 || got.Host != "172.16.0.2" || got.User == "" || got.PasswordFile == "" || got.URL != "" {
		t.Fatalf("json %+v", got)
	}
	if strings.Contains(ta.out.String(), pw) {
		t.Fatal("password in -json without -dsn")
	}
	ta.out.Reset()
	if err := ta.printUp(ctx, mc, true, true); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ta.out.Bytes(), &got); err != nil || !strings.Contains(got.URL, pw) {
		t.Fatalf("-json -dsn: %v %q", err, got.URL)
	}
	// En una terminal, -dsn pregunta; sin un sí no imprime nada.
	ta.out.Reset()
	ta.tty = true
	if err := ta.printUp(ctx, mc, false, true); err == nil || ta.out.Len() != 0 {
		t.Fatalf("printed to a terminal without asking: %v %q", err, ta.out)
	}
}
