package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// La copia embebida no puede desviarse del script del repositorio: un arreglo
// en scripts/db-golden.sh que no llegue aquí no llegaría a nadie que instale.
func TestGuionEmbebidoIgualAlDelRepo(t *testing.T) {
	repo, err := os.ReadFile("../../../../scripts/db-golden.sh")
	if err != nil {
		t.Skipf("no repository checkout: %v", err)
	}
	if !bytes.Equal(repo, guionGolden) {
		t.Fatal("ext/db/cmd/kling-db/embebido/db-golden.sh differs from scripts/db-golden.sh: run go generate ./cmd/kling-db in ext/db")
	}
}

func TestFindGoldenScriptCaeAlEmbebido(t *testing.T) {
	t.Setenv(goldenScriptEnv, "")
	p, borrar, err := findGoldenScript("")
	if err != nil {
		t.Fatal(err)
	}
	defer borrar()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, guionGolden) || !strings.HasSuffix(p, "db-golden.sh") {
		t.Fatalf("got %s, not the built-in script", p)
	}
	borrar()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the temporary script is still there: %v", err)
	}
}
