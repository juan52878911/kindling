package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// cloneDaemon contesta como un daemon: clona si le dejan, 409 si no hay
// reflink y no se pidió copia.
func cloneDaemon(reflink bool, pedidos *[]api.CloneVolumeRequest) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /volumes/{name}/clone", func(w http.ResponseWriter, r *http.Request) {
		var req api.CloneVolumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*pedidos = append(*pedidos, req)
		metodo := api.CloneReflink
		if !reflink {
			if !req.Copy {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"the host filesystem cannot clone files: operation not supported (ext4 on /var/lib/kindling/volumes). ... (kling volume clone -copy)"}`))
				return
			}
			metodo = api.CloneCopy
		}
		_ = json.NewEncoder(w).Encode(api.CloneVolumeResult{
			Volume: &api.Volume{Name: req.To, SizeBytes: 2 << 30},
			Method: metodo, Filesystem: map[bool]string{true: "xfs", false: "ext4"}[reflink], ElapsedMS: 3,
		})
	})
	return m
}

func TestVolumeCloneConReflink(t *testing.T) {
	var pedidos []api.CloneVolumeRequest
	c := fakeDaemon(t, cloneDaemon(true, &pedidos))
	var out bytes.Buffer
	if err := runVolumeClone(context.Background(), c, "datos", "rama", false, false, &out); err != nil {
		t.Fatal(err)
	}
	if len(pedidos) != 1 || pedidos[0] != (api.CloneVolumeRequest{To: "rama"}) {
		t.Fatalf("petición = %+v", pedidos)
	}
	s := out.String()
	for _, trozo := range []string{"rama  cloned from datos", "reflink", "shared with datos"} {
		if !strings.Contains(s, trozo) {
			t.Errorf("salida sin %q:\n%s", trozo, s)
		}
	}
}

// Sin reflink el error del daemon llega tal cual (dice que existe -copy), y
// con -copy la salida avisa de que fue una copia completa.
func TestVolumeCloneSinReflink(t *testing.T) {
	var pedidos []api.CloneVolumeRequest
	c := fakeDaemon(t, cloneDaemon(false, &pedidos))
	var out bytes.Buffer
	err := runVolumeClone(context.Background(), c, "datos", "rama", false, false, &out)
	if err == nil || !strings.Contains(err.Error(), "-copy") {
		t.Fatalf("debería fallar explicando -copy: %v", err)
	}
	if err := runVolumeClone(context.Background(), c, "datos", "rama", true, false, &out); err != nil {
		t.Fatal(err)
	}
	if !pedidos[1].Copy {
		t.Error("-copy no llegó al daemon")
	}
	if !strings.Contains(out.String(), "full copy") || !strings.Contains(out.String(), "ext4") {
		t.Errorf("la salida debe decir que copió y por qué:\n%s", out.String())
	}
}

func TestCloneSummary(t *testing.T) {
	bien := cloneSummary(&api.CloneInfo{Method: "reflink", Dirs: []api.CloneDir{
		{Dir: "volumes", Filesystem: "xfs", Reflink: true},
		{Dir: "machines", Filesystem: "ext4", Error: "invalid cross-device link"},
	}})
	if bien != "reflink · volumes xfs ✓ · machines ext4 ✗ invalid cross-device link" {
		t.Errorf("resumen = %q", bien)
	}
	mal := cloneSummary(&api.CloneInfo{Dirs: []api.CloneDir{{Dir: "volumes", Filesystem: "ext4", Error: "operation not supported"}}})
	if !strings.HasPrefix(mal, "none") || !strings.Contains(mal, "operation not supported") {
		t.Errorf("resumen = %q", mal)
	}
}
