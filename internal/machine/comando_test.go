package machine

import (
	"errors"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El comando de una máquina: solo en frío, validado, y al almacén MMDS junto
// al entorno bajo su propia clave.
func TestComandoDePeticion(t *testing.T) {
	if c, err := comandoDePeticion(api.RunRequest{Image: "x"}); c != nil || err != nil {
		t.Fatalf("sin comando: %v %v", c, err)
	}
	c, err := comandoDePeticion(api.RunRequest{Image: "x", Cmd: []string{"redis-server", "--port", "7000"}})
	if err != nil || c == nil || len(c.Cmd) != 3 || c.Entrypoint != nil {
		t.Fatalf("cmd: %+v %v", c, err)
	}
	if _, err := comandoDePeticion(api.RunRequest{From: "dorado", Cmd: []string{"x"}}); !errors.Is(err, ErrCmdRequest) {
		t.Fatalf("con From: %v", err)
	}
	if _, err := comandoDePeticion(api.RunRequest{Image: "x", Cmd: []string{"a\x00b"}}); !errors.Is(err, ErrCmdRequest) {
		t.Fatalf("NUL: %v", err)
	}
	if _, err := comandoDePeticion(api.RunRequest{Image: "x", Entrypoint: []string{""}}); !errors.Is(err, ErrCmdRequest) {
		t.Fatalf("entrypoint vacío: %v", err)
	}
	doc := datosArranqueMMDS(map[string]string{"A": "1"}, c)
	if _, ok := doc[api.MachineEnvMMDSKey]; !ok {
		t.Fatalf("doc = %v", doc)
	}
	if got, _ := doc[api.MachineCmdMMDSKey].(*api.MachineCmd); got == nil || got.Cmd[0] != "redis-server" {
		t.Fatalf("doc = %v", doc)
	}
	if len(datosArranqueMMDS(nil, nil)) != 0 || cmdBootArg(nil) != "" || cmdBootArg(c) != " kling.cmd=1" {
		t.Fatal("vacío o boot arg")
	}
	if comandoDe(&api.Machine{Cmd: []string{"x"}}) == nil || comandoDe(&api.Machine{}) != nil {
		t.Fatal("comandoDe")
	}
}
