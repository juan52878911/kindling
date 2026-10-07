package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestCredencialesDocker(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cfg := `{
  "auths": {
    "https://index.docker.io/v1/": {"auth": "` + b64("juan:dckr_pat_x") + `"},
    "ghcr.io": {"auth": "` + b64("juan:ghp_tok:en") + `"},
    "localhost:5000": {"username": "a", "password": "b"},
    "quay.io": {},
    "gcr.io": {"identitytoken": "x"},
    "bad.example.com": {"auth": "%%%"}
  },
  "credHelpers": {"123.dkr.ecr.eu-west-1.amazonaws.com": "ecr-login"}
}`
	creds, fuera, err := credencialesDocker([]byte(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 3 || creds["docker.io"].Password != "dckr_pat_x" || creds["ghcr.io"].Password != "ghp_tok:en" ||
		creds["localhost:5000"].Username != "a" {
		t.Fatalf("creds: %+v", creds)
	}
	todo := strings.Join(fuera, "\n")
	for _, w := range []string{"quay.io", "gcr.io (an identity token", "bad.example.com", "ecr-login"} {
		if !strings.Contains(todo, w) {
			t.Errorf("no dice que se saltó %q:\n%s", w, todo)
		}
	}
	// Ningún aviso lleva una contraseña.
	if strings.Contains(todo, "dckr_pat_x") || strings.Contains(todo, "ghp_tok") {
		t.Fatalf("un aviso lleva una contraseña:\n%s", todo)
	}

	creds, fuera, err = credencialesDocker([]byte(cfg), []string{"ghcr.io", "nope.io"})
	if err != nil || len(creds) != 1 || creds["ghcr.io"].Username != "juan" || len(fuera) != 1 || !strings.Contains(fuera[0], "nope.io (not in the file)") {
		t.Fatalf("solo ghcr.io: %+v %v %v", creds, fuera, err)
	}
	if _, _, err := credencialesDocker([]byte("no"), nil); err == nil {
		t.Fatal("un config.json roto se aceptó")
	}
}

// Sin terminal, la contraseña es stdin entero sin el salto final.
func TestLeerSecretoDeStdin(t *testing.T) {
	for in, want := range map[string]string{"tok3n\n": "tok3n", "tok3n\r\n": "tok3n", "a b c": "a b c"} {
		r, w, _ := os.Pipe()
		w.WriteString(in)
		w.Close()
		got, err := leerSecreto(r, false, "")
		r.Close()
		if err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	r, w, _ := os.Pipe()
	w.Close()
	if _, err := leerSecreto(r, false, ""); err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("vacío: %v", err)
	}
	r.Close()
}
