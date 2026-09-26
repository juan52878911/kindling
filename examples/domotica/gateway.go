package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/intent"
)

// kindling-domotica gateway: el gateway de IA de kindling (pkg/aigw, el mismo
// que `kling ai serve`) con el dominio de la habitación dentro. Las tareas de
// intención de kindling toman su dominio de un fichero de datos ("schema") o
// de uno en Go que registra el programa que embebe el gateway; la habitación
// necesita el segundo (plantillas con gramática, números en palabras en dos
// idiomas, léxico de zonas y colores), así que este programa lo registra como
// "smart-room" y ai.json lo nombra: {"intent": {"domain": "smart-room", ...}}.
//
// Todo lo demás es kindling tal cual: las réplicas de Chispa serverless, del
// codificador y del LLM las despierta y congela el planificador sobre el
// daemon de -H, la puerta de la capa 3 es `kling ai eval`, la mejora
// continua `kling ai retrain`, y cualquier cliente del gateway (kling ai …,
// la página de la habitación) le habla por su socket.

// DomainName es el nombre con el que ai.json nombra el dominio de la
// habitación.
const DomainName = "smart-room"

// domains son los dominios en Go que este gateway sirve.
func domains() (map[string]intent.Domain, error) {
	m, err := domotica.NewMatcher(domotica.DemoTemplates)
	if err != nil {
		return nil, err
	}
	return map[string]intent.Domain{DomainName: domotica.Domain{Matcher: m}}, nil
}

func runGateway(args []string) error {
	dir := filepath.Dir(config.Path())
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	host := fs.String("H", "", "kindling daemon (socket or ssh://user@host); empty = the configured one")
	cfgPath := fs.String("config", filepath.Join(dir, "ai.json"), "model and task registry (examples/domotica/ai.json)")
	socket := fs.String("socket", filepath.Join(dir, "ai.sock"), "Unix socket to listen on (mode 0600)")
	listen := fs.String("listen", "", "TCP address instead of the socket (needs a token: $KLING_AI_TOKEN or -token-file)")
	tokenFile := fs.String("token-file", filepath.Join(dir, "ai.token"), "token for -listen (0600; $KLING_AI_TOKEN wins)")
	id := fs.String("id", "domotica", "gateway id: label of its machines (ai.gateway=<id>)")
	namePrefix := fs.String("name-prefix", "gw-", "prefix of the replica machines' names")
	idle := fs.Duration("idle", 2*time.Minute, "time without requests before a replica is frozen")
	maxReplicas := fs.Int("max-replicas", 1, "replicas per model (max_replicas in the registry wins)")
	maxInflight := fs.Int("max-inflight", 1, "requests per replica before asking for another")
	pausedMiB := fs.Int("paused-mib", 256, "MiB of idle replicas kept paused instead of frozen (0 = always freeze)")
	vonTimeout := fs.Duration("von-timeout", 60*time.Second, "deadline of a layer-4 generation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var token string
	if *listen != "" {
		t, err := gatewayToken(*tokenFile)
		if err != nil {
			return fmt.Errorf("-listen needs a token: %w", err)
		}
		token = t
	}
	cfg, err := aigw.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	doms, err := domains()
	if err != nil {
		return err
	}
	h := *host
	if c, err := config.Load(); err == nil {
		h = c.Host(*host)
	}
	client := api.NewClient(h)
	g, err := aigw.New(aigw.Options{
		Client: client, ConfigPath: *cfgPath, Config: cfg, ID: *id, NamePrefix: *namePrefix,
		Idle: *idle, MaxReplicas: *maxReplicas, MaxInflight: *maxInflight, PausedMiB: *pausedMiB,
		VONTimeout: *vonTimeout, Domains: doms,
		PopularityFile: filepath.Join(dir, "ai-popularity-"+*id+".json"),
	})
	if err != nil {
		return err
	}

	var ln net.Listener
	where := *listen
	if *listen != "" {
		if ln, err = net.Listen("tcp", *listen); err != nil {
			return err
		}
	} else {
		if ln, err = listenUnix0600(*socket); err != nil {
			return err
		}
		where = *socket
		defer os.Remove(*socket)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g.Start(ctx)
	srv := &http.Server{Handler: g.Handler(token), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	// SIGHUP relee el registro sin cortar nada (como `kling ai reload`).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			notes, err := g.Reload()
			if err != nil {
				log.Printf("reload: %v", err)
				continue
			}
			log.Printf("registry reloaded")
			for _, n := range notes {
				log.Print(n)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Printf("AI gateway (domain %s) on %s; daemon %s\n", DomainName, where, client.Endpoint())
	fmt.Printf("  %d model(s), %d task(s); idle freeze after %s\n", len(cfg.Models), len(cfg.Tasks), *idle)
	for _, n := range aigw.CascadeNotes(g.Cascades()) {
		fmt.Println("  " + n)
	}
	err = srv.Serve(ln)
	// Al salir no queda ninguna réplica corriendo: se congelan todas.
	cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.Close(cctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listenUnix0600 escucha en un socket que nace 0600: quien puede abrirlo es
// el propio usuario, sin token.
func listenUnix0600(path string) (net.Listener, error) {
	if len(path) >= 104 {
		return nil, fmt.Errorf("socket path %q is %d bytes; macOS allows 103: use -socket with a shorter path", path, len(path))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another gateway is already listening on %s", path)
		}
		_ = os.Remove(path)
	}
	old := syscall.Umask(0o177) // sin ventana en la que otro lo abra
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o600)
}

// gatewayToken es el token de -listen: $KLING_AI_TOKEN o un fichero 0600 que
// ya existe (este programa no lo crea: `kling ai serve -listen` sí).
func gatewayToken(path string) (string, error) {
	t, err := token(path)
	if err != nil {
		return "", err
	}
	if len(t) < 16 {
		return "", errors.New("no token of at least 16 characters in $KLING_AI_TOKEN or " + path)
	}
	return strings.TrimSpace(t), nil
}
