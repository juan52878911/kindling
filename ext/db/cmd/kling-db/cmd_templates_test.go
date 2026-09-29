package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/templates"
)

func TestExpandTemplate(t *testing.T) {
	args, cleanup, err := expandTemplate([]string{"build", "-template", "crm-demo", "-mem", "2G", "crm"})
	if err != nil {
		t.Fatal(err)
	}
	// El nombre de la golden sigue siendo lo último; el flag ya no está.
	if args[0] != "build" || args[len(args)-1] != "crm" || indexOf(args, "-template") >= 0 {
		t.Fatalf("args %v", args)
	}
	var mig, seed string
	for i, a := range args {
		switch a {
		case "-migrations":
			mig = args[i+1]
		case "-seed":
			seed = args[i+1]
		}
	}
	if st, err := os.Stat(seed); err != nil || st.Size() == 0 {
		t.Fatalf("seed %q: %v", seed, err)
	}
	if ents, _ := filepath.Glob(filepath.Join(mig, "*.sql")); len(ents) == 0 {
		t.Fatalf("no migrations in %s", mig)
	}
	cleanup()
	if _, err := os.Stat(mig); err == nil {
		t.Fatal("cleanup left the temporary directory")
	}

	// =valor, y sin -template no toca nada.
	args, cleanup, err = expandTemplate([]string{"build", "--template=empty", "e"})
	if err != nil || !strings.Contains(strings.Join(args, " "), "-migrations") {
		t.Fatalf("%v %v", args, err)
	}
	cleanup()
	same := []string{"build", "-seed-mb", "5", "x"}
	got, cleanup, err := expandTemplate(same)
	if err != nil || strings.Join(got, " ") != strings.Join(same, " ") {
		t.Fatalf("%v %v", got, err)
	}
	cleanup()
	got, _, err = expandTemplate([]string{"image"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestExpandTemplateRechaza(t *testing.T) {
	for _, args := range [][]string{
		{"build", "-template"},
		{"build", "-template", "-keep", "x"},
		{"build", "-template=", "x"},
		{"build", "-template", "nope", "x"},
		{"build", "-template", "../empty", "x"},
		{"build", "-template", "empty/../crm-demo", "x"},
		{"build", "-template", "empty", "-template", "crm-demo", "x"},
		{"build", "-template", "empty", "-seed", "s.sql", "x"},
		{"build", "-template", "empty", "-migrations", "d", "x"},
		{"build", "-template", "empty", "-seed-mb", "5", "x"},
	} {
		if got, cleanup, err := expandTemplate(args); err == nil {
			cleanup()
			t.Errorf("%v: want error, got %v", args, got)
		}
	}
	if out, code := runExt(t, "golden", "build", "-template", "nope", "x"); code != 2 {
		t.Errorf("exit %d, want 2\n%s", code, out)
	}
}

func TestTemplatesCmd(t *testing.T) {
	out, code := runExt(t, "templates")
	if code != 0 || !strings.Contains(out, "empty") || !strings.Contains(out, "crm-demo") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, code := runExt(t, "templates", "extra"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	list, err := templates.List()
	if err != nil || len(list) < 2 {
		t.Fatalf("%v %v", list, err)
	}
}
