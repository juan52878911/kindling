package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/juan52878911/kindling-mcp/internal/gateway"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/scheduler"
	"net/http"
	"os"
	"strings"
	"time"
)

// cmdGateway y resolveGatewayToken vienen del CLI de kindling (cmd/kling/main.go).

// hostSpec es un daemon con nombre, tal como lo escribe -hosts o mcp.hosts:
// "nombre=endpoint,nombre2=endpoint2". El endpoint es lo mismo que acepta
// cfg.Host() / api.NewClient (socket unix por defecto, o unix://, ssh://).
type hostSpec struct{ Name, Endpoint string }

// parseHosts interpreta -hosts / mcp.hosts. Vacío es válido: significa "un solo
// host, el del contexto activo", que es el comportamiento de siempre.
func parseHosts(spec string) ([]hostSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []hostSpec
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, endpoint, ok := strings.Cut(part, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !ok || name == "" || endpoint == "" {
			return nil, fmt.Errorf("invalid -hosts entry %q: use name=endpoint", part)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate host name %q in -hosts", name)
		}
		seen[name] = true
		out = append(out, hostSpec{Name: name, Endpoint: endpoint})
	}
	return out, nil
}

func cmdGateway(args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	host := hostFlag(fs)
	listen := fs.String("listen", "", "where to listen (default: gateway.listen, or 127.0.0.1:8080)")
	idle := fs.Duration("idle", 0, "time without requests before freezing (default: gateway.idle, or 5m)")
	ephemeral := fs.Bool("ephemeral", false, "one microVM per action, destroyed when it's done (maximum isolation, stateless)")
	prewarm := fs.Int("prewarm", 1, "pre-warmed instances per service (0 = disabled; -ephemeral only)")
	keepwarm := fs.Int("keepwarm", 0, "N popular services with their primary warm in persistent mode (0 = disabled; avoids cold start, useful on Mac)")
	memory := fs.String("memory", "", "MCP service that remembers which tool resolved each request")
	pprofOn := fs.Bool("pprof", false, "exposes /debug/pprof; temporary diagnostics only, loopback only")
	noAuth := fs.Bool("no-auth", false, "no token; development only, and only when listening on loopback")
	hostsFlag := fs.String("hosts", "", "several daemons instead of one: name=endpoint,name2=endpoint2 (default: mcp.hosts, or one host, the active context)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	addr := config.Or(*listen, cfg.Gateway.Listen, "127.0.0.1:8080")

	// Los argumentos se validan ANTES de hablar con nadie: un flag mal puesto
	// debe fallar al instante, no después de esperar a un daemon que quizá ni
	// esté. El flag registra los perfiles, pero no los protege — si el gateway
	// escucha fuera de loopback, activarlos regala volcados de goroutines y la
	// línea de comandos a quien alcance el puerto, y deja que quien llame elija
	// cuántos segundos de CPU consume /debug/pprof/profile.
	if *pprofOn && !scheduler.IsLoopback(addr) {
		return fmt.Errorf("-pprof requires listening on loopback, and %q is not.\n"+
			"Diagnose over a tunnel:  ssh -L 8080:127.0.0.1:8080 <host>", addr)
	}

	// El token se resuelve aquí, con el resto de la validación de argumentos y
	// antes de hablar con nadie: -no-auth mal puesto debe fallar al instante, no
	// después de esperar a un daemon que quizá ni esté.
	token, err := resolveGatewayToken(cfg, *noAuth, addr)
	if err != nil {
		return err
	}

	// -hosts gana a mcp.hosts, que gana a "sin hosts": un solo host, el del
	// contexto activo, exactamente como antes de que existiera esta opción.
	hostsSpec := *hostsFlag
	if hostsSpec == "" {
		_, _ = cfg.Extension("mcp", "hosts", &hostsSpec)
	}
	hosts, err := parseHosts(hostsSpec)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		hosts = []hostSpec{{Name: "default", Endpoint: cfg.Host(*host)}}
	}

	wait := *idle
	if wait == 0 {
		wait, _ = time.ParseDuration(cfg.Gateway.Idle)
	}
	if wait == 0 {
		wait = 5 * time.Minute
	}
	listen, idle = &addr, &wait

	memSvc := *memory
	if on, svc := memoryConfig(cfg); memSvc == "" && on {
		memSvc = svc
	}

	// Cuotas por token/tenant, si la configuración las trae. Retrocompatible: sin
	// tokens con nombre, el token único sigue siendo el tenant "default" sin
	// límites. Es reparto justo, no una frontera de seguridad (todo comparte
	// daemon y bridge). Se calculan una vez y se aplican a CADA host: la
	// configuración es global, no por host.
	var tenants []scheduler.TenantLimit
	if len(cfg.Gateway.Tokens) > 0 {
		tenants = make([]scheduler.TenantLimit, 0, len(cfg.Gateway.Tokens))
		for _, t := range cfg.Gateway.Tokens {
			tenants = append(tenants, scheduler.TenantLimit{
				Name:         t.Name,
				Token:        t.Token,
				MaxInstances: t.MaxInstances,
				MaxInflight:  t.MaxInflight,
			})
		}
	}

	var routerHosts []gateway.RouterHost
	for _, h := range hosts {
		c := api.NewClient(h.Endpoint)
		if _, err := c.Info(ctx); err != nil {
			return fmt.Errorf("can't reach host %q (%s): %w", h.Name, h.Endpoint, err)
		}
		gw := gateway.New(c, *idle, *ephemeral, *prewarm, memSvc)
		gw.KeepWarm = *keepwarm
		if len(tenants) > 0 {
			gw.SetTenants(tenants)
		}
		go gw.Reap(ctx)
		if *ephemeral {
			go gw.PrewarmAll(ctx)
		}
		// Calienta ya al arrancar, sin esperar al primer tick del segador
		// (idle/3): así los servicios populares están listos antes de la
		// primera petición. No se ata a -ephemeral a propósito —el keep-warm
		// es justo para el modo persistente—.
		if *keepwarm > 0 {
			go gw.KeepWarmAll(ctx)
		}
		// Contexto propio y ACOTADO: no puede ser el del proceso, que ya está
		// cancelado cuando llega el apagado (los Remove no se harían), ni uno
		// sin límite, que dejaría a Ctrl-C esperando indefinidamente a un
		// daemon que no responde. Retirar las pre-calentadas es deseable, no
		// obligatorio.
		defer func(gw *gateway.Gateway) {
			dc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			gw.Drain(dc)
		}(gw)
		routerHosts = append(routerHosts, gateway.RouterHost{Name: h.Name, GW: gw})
	}

	// Sin -hosts/mcp.hosts, esto es UN host: se usa su Handler tal cual, así
	// que el comportamiento no cambia ni un byte respecto de antes de que
	// existiera Router. Con más de uno, Router decide a cuál va cada petición
	// y le delega el resto.
	var handler http.Handler
	if len(routerHosts) == 1 {
		gw := routerHosts[0].GW
		gw.PprofEnabled = *pprofOn
		handler = gw.Handler(token)
	} else {
		router := gateway.NewRouter(routerHosts)
		router.PprofEnabled = *pprofOn
		go router.Reap(ctx)
		handler = router.Handler(token)
	}

	srv := &http.Server{
		Addr:    *listen,
		Handler: handler,
		// Mismas razones que en el daemon: gateway es la única superficie
		// que escucha en TCP, y un cliente que no termina el header
		// mantiene goroutine + FD indefinidamente. /mcp/{svc} puede ser
		// streaming (SSE), así que ReadTimeout va holgado.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()

	// El gateway escucha en red; el daemon no. Por defecto solo en loopback:
	// abrirlo al mundo debe ser una decisión consciente.
	fmt.Printf("gateway at http://%s\n", *listen)
	fmt.Printf("  tool:         http://%s/mcp/<service>\n", *listen)
	fmt.Printf("  inventory:    http://%s/services\n", *listen)
	fmt.Printf("  idle:         %s before freezing\n", *idle)
	if len(routerHosts) > 1 {
		fmt.Printf("  hosts:        %d\n", len(routerHosts))
		// routerHosts se construyó recorriendo hosts en orden y sin saltarse
		// ninguno (un fallo de -h corta con error antes de llegar aquí), así
		// que los índices coinciden uno a uno.
		for i, h := range routerHosts {
			fmt.Printf("                %-12s %s\n", h.Name, hosts[i].Endpoint)
		}
	}
	if token == "" {
		fmt.Printf("  auth:         DISABLED (-no-auth) — only valid because it listens on loopback\n")
	} else {
		fmt.Printf("  auth:         Authorization: Bearer <token>  ·  /healthz open\n")
	}
	if *pprofOn {
		fmt.Printf("  pprof:        ACTIVE at http://%s/debug/pprof/ (behind the token) — turn it off when done\n", *listen)
	}
	if memSvc != "" {
		fmt.Printf("  memory:       active on %q — ranks searches by what already worked\n", memSvc)
	}
	if *ephemeral {
		fmt.Printf("  mode:         EPHEMERAL — each action in its own microVM, destroyed when done\n")
		if *prewarm > 0 {
			fmt.Printf("  pre-warmed:   %d instance(s) per service, ready to respond\n", *prewarm)
		}
	}
	if *keepwarm > 0 {
		fmt.Printf("  keep-warm:    %d popular service(s) with their primary warm (no cold start)\n", *keepwarm)
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// resolveGatewayToken decide con qué token arranca el gateway.
//
// No hay flag `-token` a propósito: la línea de comandos de un proceso la lee
// cualquier usuario del host en /proc, así que un secreto no puede viajar por
// ahí. Variable de entorno para systemd, fichero de configuración para el resto,
// y si no hay ninguno se genera y se guarda: que el gateway quede abierto no
// puede ser lo que pasa cuando no configuras nada.
func resolveGatewayToken(cfg *config.Config, noAuth bool, addr string) (string, error) {
	if noAuth {
		if !scheduler.IsLoopback(addr) {
			return "", fmt.Errorf("-no-auth requires listening on loopback, and %q is not.\n"+
				"Waking a snapshot means executing code: without a token, anyone who reaches\n"+
				"that port runs your tools. Remove -no-auth or listen on 127.0.0.1", addr)
		}
		return "", nil
	}
	if t := os.Getenv("KLING_GATEWAY_TOKEN"); t != "" {
		return t, nil
	}
	if cfg.Gateway.Token != "" {
		return cfg.Gateway.Token, nil
	}

	t, err := scheduler.NewToken()
	if err != nil {
		return "", err
	}
	cfg.Gateway.Token = t

	// Si no se puede guardar NO se aborta: un gateway que se niega a arrancar
	// porque no pudo persistir un token es peor que uno que arranca y avisa.
	// Pasa con systemd, donde ProtectHome deja su configuración en solo lectura,
	// y ahí lo grave no es el fallo sino el silencio: el token cambiaría en cada
	// reinicio y todos los agentes ya configurados dejarían de entrar.
	if err := cfg.Save(); err != nil {
		fmt.Printf("\nWARNING: I generated a token but couldn't save it to %s (%v).\n", config.Path(), err)
		fmt.Printf("       It WILL CHANGE on every restart. Pin it so that doesn't happen:\n")
		fmt.Printf("         Environment=KLING_GATEWAY_TOKEN=%s\n\n", t)
		return t, nil
	}

	// La única vez que se imprime entero. A partir de aquí `config show` lo
	// enmascara, porque esa orden se teclea con gente mirando la pantalla.
	fmt.Printf("token generated and saved to %s\n\n", config.Path())
	fmt.Printf("  On the machine where you use the CLI:\n")
	fmt.Printf("    kling config set gateway.token %s\n\n", t)
	return t, nil
}

// ── máquinas ──────────────────────────────────────────────────────────────────
