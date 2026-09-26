package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

type cierreVacio struct{}

func (cierreVacio) Write(p []byte) (int, error) { return len(p), nil }
func (cierreVacio) Close() error                { return nil }

// Una respuesta de mas de 16 MiB hacia que el escaner se rindiera, la sesion
// muriera entera, y el log dijera "MCP server died". Un diagnostico FALSO: manda
// a mirar al servidor MCP, que estaba perfecto — lo que no cabia era su
// respuesta.
//
// Y no es un caso raro con este sistema: una captura de pantalla de un servicio
// de navegador viaja en base64 dentro del JSON-RPC.
func TestUnaLineaDemasiadoLargaNoSeConfundeConUnServidorMuerto(t *testing.T) {
	s := &session{
		id:      "0123456789abcdef",
		cmd:     exec.Command("true"), // sin arrancar: close() no matara nada
		stdin:   cierreVacio{},
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 1),
		done:    make(chan struct{}),
		exitCh:  make(chan syscall.WaitStatus, 1),
	}

	// 17 MiB en una linea, y detras una respuesta perfectamente normal que ya
	// no llegara: ese es el dano real, no solo el mensaje confuso.
	entrada := strings.NewReader(
		strings.Repeat("x", 17<<20) + "\n" +
			`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")

	s.readLoop(entrada)

	s.mu.Lock()
	motivo := s.scanErr
	s.mu.Unlock()

	if motivo == nil {
		t.Fatal("el escaner se rindio y nadie lo registro: se seguiria reportando como servidor muerto")
	}
	if !strings.Contains(motivo.Error(), "too long") {
		t.Errorf("el motivo no dice que la linea no cabia: %v", motivo)
	}
}

// Y el cierre normal —el servidor termina y suelta su salida— NO es un error:
// marcarlo lo seria convertiria un apagado limpio en una alarma.
func TestUnCierreNormalNoDejaMotivoDeError(t *testing.T) {
	s := &session{
		id:      "0123456789abcdef",
		cmd:     exec.Command("true"),
		stdin:   cierreVacio{},
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 1),
		done:    make(chan struct{}),
		exitCh:  make(chan syscall.WaitStatus, 1),
	}
	s.readLoop(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))

	s.mu.Lock()
	motivo := s.scanErr
	s.mu.Unlock()
	if motivo != nil {
		t.Errorf("un cierre limpio dejo motivo de error: %v", motivo)
	}
}
