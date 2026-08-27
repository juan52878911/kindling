package main

// Modo navegador COMPARTIDO, con arranque PEREZOSO.
//
// Un servidor MCP de navegador (Playwright, Puppeteer) lanza un Chromium, y
// Chromium pesa cientos de MB. El modelo por defecto del puente —un proceso por
// sesión— daría un Chromium por sesión: caro y sin manera de que dos agentes
// compartan un navegador.
//
// Cuando la construcción de la imagen detecta la dependencia de Chromium, deja
// un marcador (/etc/kling/browser.json). Con él, el puente arranca UN solo
// Chromium con puerto de depuración (el "punto común") y a cada sesión le añade
// el argumento que la conecta a ese Chromium por CDP con su PROPIO contexto.
//
// PEREZOSO a propósito, por la filosofía serverless: el Chromium NO arranca al
// bootear, sino en la primera sesión que lo necesita, y se detiene al resetear.
// Así:
//   - el snapshot dorado se congela SIN Chromium dentro → mem.file ligero, thaw
//     más rápido, el guard reserva menos;
//   - una microVM viva pero entre sesiones no retiene ~500 MB de navegador;
//   - cuando de verdad hace falta, arranca en ~2 s y N sesiones lo reusan.
//
// El puente no sabe (ni le importa) que es Chromium: solo arranca lo que diga
// `sidecar`, espera a `ready_url` y añade `session_args`. Vale igual para
// Puppeteer o cualquier navegador futuro.

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

// launch arranca el Chromium compartido y espera a que su puerto de depuración
// responda. Si no llega a estar listo, mata el proceso y devuelve error: mejor
// fallar aquí, donde se ve en la consola serie, que como un CDP roto más tarde.
func (s *browserSpec) launch(env []string) (*exec.Cmd, error) {
	cmd := exec.Command(s.Sidecar[0], s.Sidecar[1:]...)
	cmd.Env = env
	// La salida del navegador va a la consola serie de la microVM, como la del
	// servidor MCP: si Chromium se queja (sandbox, /dev/shm), se lee ahí.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Chromium lanza un arbol de procesos (zygote, gpu, renderers). En su propio
	// grupo se puede matar entero; sin esto quedaban vivos al cerrar.
	enSuPropioGrupo(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching shared Chromium: %w", err)
	}
	log.Printf("browser: shared Chromium launched (pid %d)", cmd.Process.Pid)
	// No hacemos Wait: es de por vida. Cuando muera, el cosechador (procReaper,
	// wait4 de PID 1) lo recoge como huérfano.

	if s.ReadyURL == "" {
		return cmd, nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.ReadyURL)
		if err == nil {
			resp.Body.Close()
			return cmd, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	matarGrupo(cmd)
	return nil, fmt.Errorf("shared Chromium did not open %s within 40s", s.ReadyURL)
}

// ensureBrowser arranca el Chromium compartido si aún no corre. Se llama cuando
// llega la primera sesión (initialize), no al bootear: ese es el arranque
// perezoso. Bajo su propio candado, para no bloquear la tabla de sesiones
// durante los ~2 s que tarda el navegador en levantar.
func (b *bridge) ensureBrowser() error {
	if b.browser == nil {
		return nil
	}
	b.bmu.Lock()
	defer b.bmu.Unlock()
	if b.bproc != nil {
		return nil // ya vivo, se reusa
	}
	cmd, err := b.browser.launch(b.env)
	if err != nil {
		return err
	}
	b.bproc = cmd
	return nil
}

// stopBrowser mata el Chromium compartido si corre. Lo llama /reset —para que el
// commit del snapshot dorado se congele SIN navegador— y el cierre de la última
// sesión, para no retener el navegador entre trabajos.
func (b *bridge) stopBrowser() {
	if b.browser == nil {
		return
	}
	b.bmu.Lock()
	defer b.bmu.Unlock()
	if b.bproc != nil {
		matarGrupo(b.bproc)
		b.bproc = nil
		log.Printf("browser: shared Chromium stopped")
	}
}

// stopBrowserIfIdle detiene el navegador solo si no queda ninguna sesión viva.
// Se llama tras cerrar una sesión: si era la última, libera los ~500 MB.
func (b *bridge) stopBrowserIfIdle() {
	if b.browser == nil {
		return
	}
	b.mu.Lock()
	n := len(b.sessions)
	b.mu.Unlock()
	if n == 0 {
		b.stopBrowser()
	}
}
