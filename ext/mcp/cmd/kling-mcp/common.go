package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Ayudantes compartidos con el CLI de kindling. Son copias: una extensión no
// importa el paquete main del núcleo.

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// hostFlag registra -H en cualquier subcomando del cliente.
//
// Por defecto vacío a propósito: así hostOf() distingue "no me lo han dicho" de
// "me han dicho esto", y puede aplicar la precedencia
// -H > $KLING_HOST > contexto activo > socket local.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("H", "", "daemon endpoint (socket or ssh://user@host)")
}

// loadConfig lee la configuración sin hacer fallar al CLI si está corrupta: una
// configuración ilegible no debe impedir listar máquinas.
func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (falling back to defaults)\n", err)
		return &config.Config{}
	}
	return cfg
}

// hostOf resuelve a qué daemon hablar.
func hostOf(flagValue string) string { return loadConfig().Host(flagValue) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ── daemon ────────────────────────────────────────────────────────────────────

// human formatea bytes de forma compacta.
func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dM", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dK", b>>10)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func since(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// reorderFor mueve los flags delante de los posicionales para que un flag escrito
// DESPUÉS de un argumento posicional no se ignore en silencio (el paquete `flag`
// de Go deja de parsear al primer no-flag). Es la versión correcta de reorder:
//   - Consulta el flagset para saber qué flags son booleanos (y por tanto NO se
//     llevan el siguiente argumento), en vez de una lista hardcodeada.
//   - Se detiene en `--`: todo lo que sigue es el comando del servidor y se deja
//     intacto, sin reordenar.
//
// Los flags SÍ deben estar definidos en fs antes de llamar aquí (lo están: se
// define todo y luego se parsea).
func reorderFor(fs *flag.FlagSet, args []string) []string {
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		bf, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && bf.IsBoolFlag()
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Fin de las opciones: el resto es el comando del servidor.
			out := append(flags, positional...)
			return append(out, args[i:]...)
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) &&
				(len(args[i+1]) == 0 || args[i+1][0] != '-') && !isBool(a) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// ── configuración general ─────────────────────────────────────────────────────

func gatewayServiceNames(url, token string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names, nil
}

// mcpHealthLine resume el último sondeo de salud de los servicios importados.
//
// Solo LEE los veredictos que dejó `kling mcp health` en el meta de cada
// snapshot: sondear aquí arrancaría una microVM por servicio y `status` debe
// ser instantáneo. Por eso "never probed" es un estado que se muestra y no se
// disimula: sin sondeo no hay dato, y fingir salud es lo que tapó la caída.
func mcpHealthLine(snaps []*api.Snapshot) string {
	var healthy, unknown int
	var sick []string
	for _, s := range snaps {
		switch mcp.HealthOf(s).Status {
		case mcp.Healthy:
			healthy++
		case mcp.Unhealthy:
			n := s.Name
			if svc := s.Service(); svc != "" {
				n = svc
			}
			sick = append(sick, n)
		default:
			unknown++
		}
	}
	switch {
	case len(sick) > 0:
		line := fmt.Sprintf("✗ %d unhealthy (%s) · %d healthy", len(sick), strings.Join(sick, ", "), healthy)
		if unknown > 0 {
			line += fmt.Sprintf(" · %d never probed", unknown)
		}
		return line + " — details: kling mcp ls"
	case unknown == len(snaps):
		return fmt.Sprintf("? none of the %d service(s) has ever been probed — probe them: kling mcp health", unknown)
	case unknown > 0:
		return fmt.Sprintf("✓ %d healthy · %d never probed — probe them: kling mcp health", healthy, unknown)
	default:
		return fmt.Sprintf("✓ %d healthy", healthy)
	}
}

// servicesLine resume /services, que responde texto plano, una línea por
// servicio.
func servicesLine(url, token string) string {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "✗ " + err.Error()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "✗ " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if token == "" {
			return "✗ requires a token and none is configured here\n" +
				"              copy it from the host:  kling config set gateway.token <t>"
		}
		return "✗ the gateway rejects the token from gateway.token (401)"
	}
	if resp.StatusCode >= 300 {
		return "✗ HTTP " + resp.Status
	}

	// Acotado: leer sin límite una respuesta ajena es regalarle a quien esté al
	// otro lado la memoria de este proceso. 64 KiB dan para miles de servicios.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	if len(names) == 0 {
		return "✓ none yet — package one with:  kling add <server>"
	}
	return fmt.Sprintf("✓ %d: %s", len(names), strings.Join(names, ", "))
}

// ── utilidades ────────────────────────────────────────────────────────────────

func markYes(ok bool, yes, no string) string {
	if ok {
		return "✓ " + yes
	}
	return "✗ " + no
}

// httpOK comprueba que una URL responde 2xx. Se usa con /healthz, que está
// abierto por diseño y por tanto no necesita token.
func httpOK(url string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

// La memoria de uso del gateway se configura en extensions.mcp de config.json:
// memory.enabled y memory.service, las dos claves que declara el manifiesto
// (`kling config set mcp.memory.service engram`). Hasta v0.5 era la sección
// "memory" del núcleo, que kindling eleva aquí al cargar.

// memoryConfig devuelve si la memoria de uso está activa y sobre qué servicio.
func memoryConfig(cfg *config.Config) (enabled bool, service string) {
	_, _ = cfg.Extension("mcp", "memory.enabled", &enabled)
	_, _ = cfg.Extension("mcp", "memory.service", &service)
	return enabled, service
}

// setMemoryConfig la cambia; service vacío deja el que hubiera.
func setMemoryConfig(cfg *config.Config, enabled bool, service string) error {
	if err := cfg.SetExtension("mcp", "memory.enabled", "bool", strconv.FormatBool(enabled)); err != nil {
		return err
	}
	if service != "" {
		return cfg.SetExtension("mcp", "memory.service", "string", service)
	}
	return nil
}
