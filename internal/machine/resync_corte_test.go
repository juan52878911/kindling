package machine

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteQueCorta acepta conexiones, lee la petición y las cierra sin
// contestar: un agente que estaba y se cae bajo carga (EOF), no un puerto
// vacío.
func agenteQueCorta(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Lee la petición entera (cabeceras y cuerpo) y cierra sin
			// contestar: el cliente ve un EOF.
			_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, _ = io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// S4: un corte sin respuesta no se recuerda como "sin agente" para el
// snapshot: la copia siguiente vuelve a intentarlo.
func TestResyncCorteNoSeRecuerdaComoSinAgente(t *testing.T) {
	addr := agenteQueCorta(t)
	m := &Manager{byID: map[string]*api.Machine{
		"x": {ID: "x", Name: "x", Image: "img", Forwards: map[string]string{"8080": addr}},
	}}
	capturarLog(t)
	snap := &api.Snapshot{Name: "dorado", CreatedAt: time.Now()}
	clave := claveSnapshot(snap)
	m.resyncGuest(context.Background(), "x", clave, api.ResyncInstance)
	if _, ok := m.resyncSinAgente.Load(clave); ok {
		t.Fatal("un corte del agente se recordó como snapshot sin agente")
	}
	// Un rechazo (nadie en el puerto) sí.
	m.byID["x"].Forwards["8080"] = dirSinNadie
	m.resyncGuest(context.Background(), "x", clave, api.ResyncInstance)
	if _, ok := m.resyncSinAgente.Load(clave); !ok {
		t.Fatal("un puerto vacío ya no se recuerda")
	}
}

// S4: la copia de un dorado guardado con el agente escuchando no se entrega
// si nadie contesta, ni por un corte ni por un puerto vacío.
func TestResyncCopiaConAgenteNoSeEntregaSinRespuesta(t *testing.T) {
	for nombre, addr := range map[string]string{"corte": agenteQueCorta(t), "puerto vacío": dirSinNadie} {
		t.Run(nombre, func(t *testing.T) {
			m := &Manager{byID: map[string]*api.Machine{
				"x": {ID: "x", Name: "x", Image: "img", Forwards: map[string]string{"8080": addr}},
			}}
			capturarLog(t)
			snap := &api.Snapshot{Name: "dorado", CreatedAt: time.Now(), ConAgente: true}
			if _, err := m.resyncCopia(context.Background(), "x", snap); err == nil {
				t.Fatal("se entregó una copia cuyo agente no contestó")
			}
			// Sin el campo (un dorado anterior), como antes: se entrega.
			snap.ConAgente = false
			if _, err := m.resyncCopia(context.Background(), "x", snap); err != nil {
				t.Fatalf("sin con_agente: %v", err)
			}
		})
	}
}
