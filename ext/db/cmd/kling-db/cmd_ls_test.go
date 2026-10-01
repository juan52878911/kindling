package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// ls enseña solo las copias del dueño, y marca la retenida por el almacén y
// la que vio errores de disco, con qué hacer.
func TestLsAvisaDeDiscoYRetencion(t *testing.T) {
	ta := newTestApp(t)
	db := func(golden, owner string) map[string]string {
		return map[string]string{labelGolden: golden, labelOwner: owner, labelState: api.DBStateReady}
	}
	ta.f.newMachine("sana", db("pg", defaultOwner))
	ret := ta.f.newMachine("retenida", db("pg", defaultOwner))
	ret.State, ret.Hold = api.StatePaused, api.HoldStoreFull
	rota := ta.f.newMachine("rota", db("aura-main", defaultOwner))
	rota.DiskErrors, rota.DiskError = 2, "I/O error, dev vdb, sector 0"
	ta.f.newMachine("ajena", db("pg", "otro"))
	ta.f.newMachine("suelta", nil)

	if err := ta.ls(context.Background(), defaultOwner, false); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	t.Log("\n" + out)
	for _, w := range []string{"sana", "retenida", "paused!", "rota", "running!", "on hold", "kling cow grow", "disk I/O error", "kling db doctor rota"} {
		if !strings.Contains(out, w) {
			t.Errorf("falta %q", w)
		}
	}
	for _, w := range []string{"ajena", "suelta"} {
		if strings.Contains(out, w) {
			t.Errorf("sobra %q", w)
		}
	}

	ta.out.Reset()
	if err := ta.ls(context.Background(), defaultOwner, true); err != nil {
		t.Fatal(err)
	}
	var filas []copiaLs
	if err := json.Unmarshal(ta.out.Bytes(), &filas); err != nil || len(filas) != 3 {
		t.Fatalf("json: %v %+v", err, filas)
	}
}
