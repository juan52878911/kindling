package main

// Modo navegador COMPARTIDO.
//
// Un servidor MCP de navegador (Playwright, Puppeteer) lanza un Chromium, y
// Chromium pesa cientos de MB. El modelo por defecto del puente —un proceso por
// sesión— daría un Chromium por sesión: caro y sin manera de que dos agentes
// compartan un navegador.
//
// Cuando la construcción de la imagen detecta la dependencia de Chromium, deja
// un marcador (/etc/kling/browser.json). Con él, el puente arranca UN solo
// Chromium con puerto de depuración (el "punto común") ANTES de servir nada, y a
// cada sesión le añade el argumento que la conecta a ese Chromium por CDP. Así:
//
//   - hay UN navegador, no uno por sesión;
//   - cada sesión abre su PROPIO contexto (cookies/almacenamiento aislados) y sus
//     páginas dentro de ese navegador;
//   - el enrutado por Mcp-Session-Id que ya hace el puente basta para que cada
//     conjunto de herramientas caiga en su sesión, y por tanto en su contexto.
//
// Todo lo dispara el marcador; el puente no sabe (ni le importa) que es
// Chromium: solo arranca lo que diga `sidecar`, espera a `ready_url` y añade
// `session_args` a cada sesión.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const browserSpecPath = "/etc/kling/browser.json"

// browserSpec es el marcador que hornea la imagen de un servicio de navegador.
type browserSpec struct {
	Sidecar     []string `json:"sidecar"`      // cómo arrancar el Chromium compartido
	ReadyURL    string   `json:"ready_url"`    // GET que responde cuando ya está listo
	SessionArgs []string `json:"session_args"` // qué añadir al comando de cada sesión
}

// loadBrowserSpec lee el marcador. Devuelve nil si no existe o está vacío: la
// inmensa mayoría de los servicios no son de navegador y no deben pagar nada.
func loadBrowserSpec() *browserSpec {
	b, err := os.ReadFile(browserSpecPath)
	if err != nil {
		return nil
	}
	var s browserSpec
	if json.Unmarshal(b, &s) != nil || len(s.Sidecar) == 0 {
		return nil
	}
	return &s
}

// start lanza el Chromium compartido y espera a que su puerto de depuración
// responda. Va ANTES de servir: si el navegador común no está listo, la primera
// sesión que intente conectarse por CDP fallaría, y es mejor verlo aquí —en la
// consola serie— que como un handshake roto más tarde.
func (s *browserSpec) start(env []string) error {
	cmd := exec.Command(s.Sidecar[0], s.Sidecar[1:]...)
	cmd.Env = env
	// La salida del navegador va a la consola serie de la microVM, como la del
	// servidor MCP: si Chromium se queja (sandbox, /dev/shm), se lee ahí.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lanzando el Chromium compartido: %w", err)
	}
	log.Printf("navegador: Chromium compartido lanzado (pid %d)", cmd.Process.Pid)
	// No hacemos Wait: es un proceso de por vida. Cuando muera, el cosechador
	// (procReaper, wait4 de PID 1) lo recoge como huérfano.

	if s.ReadyURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.ReadyURL)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("el Chromium compartido no abrió %s en 40s", s.ReadyURL)
}
