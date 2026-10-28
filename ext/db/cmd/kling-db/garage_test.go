package main

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// fakeGarage es un Kling que imita la CLI de Garage de una copia: guarda las
// claves y registra cada orden.
type fakeGarage struct {
	backend // solo Run se usa
	keys    []string
	calls   []string
	fail    string // subcomando que falla ("key import", ...)
}

func (f *fakeGarage) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	// exec -timeout 60s ID -- /garage -c /etc/garage.toml <cmd...>
	i := 0
	for i < len(args) && args[i] != "--" {
		i++
	}
	cmd := args[i+4:]
	f.calls = append(f.calls, strings.Join(cmd, " "))
	sub := cmd[0] + " " + cmd[1]
	if sub == f.fail {
		return nil, io.ErrUnexpectedEOF
	}
	switch sub {
	case "key list":
		return []byte("List of keys:\n  " + strings.Join(f.keys, "\n  ") + "\n"), nil
	case "key import":
		f.keys = append(f.keys, cmd[len(cmd)-2])
	case "key delete":
		id := cmd[len(cmd)-1]
		for j, k := range f.keys {
			if k == id {
				f.keys = append(f.keys[:j], f.keys[j+1:]...)
				break
			}
		}
	}
	return nil, nil
}

func TestS3AccessKey(t *testing.T) {
	pw := strings.Repeat("ab", 32)
	ak := dbstate.S3AccessKey(pw)
	if !regexp.MustCompile(`^GK[0-9a-f]{24}$`).MatchString(ak) {
		t.Fatalf("forma %q", ak)
	}
	if dbstate.S3AccessKey(pw) != ak || dbstate.S3AccessKey(strings.Repeat("cd", 32)) == ak {
		t.Fatal("la clave de acceso no es función del secreto")
	}
}

func TestEngineDSNGarage(t *testing.T) {
	pw := strings.Repeat("ab", 32)
	got := engineDSN(engineGarage, "app", pw, "10.0.0.5", 3900, "seed")
	want := "s3://" + dbstate.S3AccessKey(pw) + ":" + pw + "@10.0.0.5:3900/seed"
	if got != want {
		t.Fatalf("%s, quería %s", got, want)
	}
	if !knownEngine(engineGarage) || enginePort(engineGarage) != 3900 || clientMode(engineGarage) != "dsn" ||
		pgBacked(engineGarage) || !hasPassword(engineGarage) || engineOf(map[string]string{labelEngine: "garage"}) != engineGarage {
		t.Fatal("garage no se reconoce como motor con clave propia")
	}
}

// La rotación deja una sola clave (la nueva), borra la del dorado y no mete el
// secreto en el error de una orden que falla.
func TestSetGarageKeyRota(t *testing.T) {
	golden := dbstate.S3AccessKey("gold")
	f := &fakeGarage{keys: []string{golden}}
	a := &app{k: f}
	pw := strings.Repeat("0f", 32)
	if err := a.setGarageKey(context.Background(), "0123456789abcdef", "app", "seed", pw); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || f.keys[0] != dbstate.S3AccessKey(pw) {
		t.Fatalf("claves tras rotar: %v", f.keys)
	}
	// Rotar otra vez: la anterior desaparece, no se acumulan.
	pw2 := strings.Repeat("a1", 32)
	if err := a.setGarageKey(context.Background(), "0123456789abcdef", "app", "seed", pw2); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || f.keys[0] != dbstate.S3AccessKey(pw2) {
		t.Fatalf("claves tras la segunda rotación: %v", f.keys)
	}

	f = &fakeGarage{keys: []string{golden}, fail: "key import"}
	a = &app{k: f}
	err := a.setGarageKey(context.Background(), "0123456789abcdef", "app", "seed", pw)
	if err == nil || strings.Contains(err.Error(), pw) {
		t.Fatalf("error %v", err)
	}
	if len(f.keys) != 1 || f.keys[0] != golden {
		t.Fatal("una rotación fallida no debe tocar la clave que había")
	}
}

func TestSetGarageKeyRechazaEntradasRaras(t *testing.T) {
	a := &app{k: &fakeGarage{}}
	pw := strings.Repeat("0f", 32)
	for _, c := range []struct{ user, bucket, pw string }{
		{"app; rm", "seed", pw}, {"app", "Se ed", pw}, {"app", "seed", "corto"},
	} {
		if err := a.setGarageKey(context.Background(), "0123456789abcdef", c.user, c.bucket, c.pw); err == nil {
			t.Errorf("%+v: debería rechazarse", c)
		}
	}
}
