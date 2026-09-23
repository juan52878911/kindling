// kling-guest es el agente genérico de invitado de kindling: el PID 1 de una
// microVM que no aloja un servidor MCP.
//
// Monta los volúmenes, recoge huérfanos, prepara la ruta a MMDS y sirve
// /healthz, /dns y /volume/*; y /exec si el kernel arrancó con kling.exec=1.
// Es lo que llevan las imágenes de herramientas (`kling images toolchain`) y
// lo que llevarán los sandboxes de código. El puente MCP, kling-bridge, embebe
// el mismo agente (pkg/guest) y añade encima sus rutas.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
)

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	listen := flag.String("listen", ":8080", "where to listen")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "kling-guest — kindling's guest agent (PID 1 inside a microVM)\n\n  kling-guest [options]\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}

	// PID 1 no se deja matar: al recibir SIGTERM ordena el apagado y desmonta
	// los volúmenes, o lo último escrito se perdería con la máquina.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	agent, err := guest.New()
	if err != nil {
		log.Fatalf("volume: %v", err)
	}
	mux := http.NewServeMux()
	agent.Register(mux)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Sin WriteTimeout, como el puente: un /exec puede tardar minutos con
		// toda legitimidad y cortarlo a medias es peor que esperar.
	}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(done)
	}()

	log.Printf("kling-guest %s listening on %s", Version, *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
	agent.Close()
}
