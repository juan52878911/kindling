// kling-guest es el agente genérico de invitado de kindling: el PID 1 de una
// microVM que no aloja un servidor MCP.
//
// Monta los volúmenes, recoge huérfanos, prepara la ruta a MMDS y sirve
// /healthz, /dns y /volume/*; y /exec si el kernel arrancó con kling.exec=1.
// Es lo que llevan las imágenes de herramientas (`kling images toolchain`) y
// lo que llevarán los sandboxes de código. El puente MCP, kling-bridge, embebe
// el mismo agente (pkg/guest) y añade encima sus rutas.
//
// Llamado como overlay-init (un enlace en /sbin, en las imágenes de Docker
// sin sh), es el init de la microVM: monta el overlay, hace pivot_root y se
// vuelve a ejecutar como agente (pkg/guest/init.go).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
)

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	if filepath.Base(os.Args[0]) == "overlay-init" {
		guest.Init() // no vuelve
	}
	listen := flag.String("listen", ":8080", "where to listen")
	version := flag.Bool("version", false, "print the version and exit")
	// La sonda de "listo" de las imágenes del constructor oci: que el puerto
	// de EXPOSE acepte conexiones. Aquí y no con nc, que no todas traen.
	probeTCP := flag.String("probe-tcp", "", "exit 0 if host:port accepts a TCP connection, 1 if not (a readiness probe)")
	// Las sondas de las imágenes sin sh: un #! que apunta aquí con el argv en
	// JSON en la segunda línea del fichero (el HEALTHCHECK CMD de la imagen).
	execJSON := flag.Bool("exec-json", false, "run the JSON argv on the second line of the given file (a #! probe for images without sh)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "kling-guest — kindling's guest agent (PID 1 inside a microVM)\n\n  kling-guest [options]\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}
	if *execJSON {
		if err := execArgvFile(flag.Arg(0)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *probeTCP != "" {
		c, err := net.DialTimeout("tcp", *probeTCP, 2*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c.Close()
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
	agent.Name, agent.Version = "kling-guest", Version
	mux := http.NewServeMux()
	agent.Register(mux)
	// El servicio de la imagen, si declara uno (/etc/kindling/service.json):
	// con los volúmenes ya montados.
	agent.StartService()

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// ReadHeaderTimeout y no ReadTimeout: éste corta la conexión entera
		// (headers + cuerpo) a los 120 s, y una subida de PUT /files por un
		// túnel lento pide 15 minutos propios (ver files.go). Sin límite en la
		// cabecera, un cliente que abre la conexión y nunca la termina se queda
		// con una goroutine y un FD para siempre.
		ReadHeaderTimeout: 120 * time.Second,
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

// execArgvFile ejecuta, en lugar de este proceso, el argv de p
// (readArgvFile), buscando el programa en el PATH, como el HEALTHCHECK CMD de
// Docker. Solo vuelve si no puede.
func execArgvFile(p string) error {
	argv, err := readArgvFile(p)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(bin, argv, os.Environ())
}

// readArgvFile lee el argv en JSON de la segunda línea de p (la primera es el
// #!).
func readArgvFile(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(io.LimitReader(f, 64<<10))
	if _, err := r.ReadString('\n'); err != nil {
		return nil, fmt.Errorf("%s: no argv line", p)
	}
	var argv []string
	if err := json.NewDecoder(r).Decode(&argv); err != nil || len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("%s: the second line must be a JSON array with the command", p)
	}
	return argv, nil
}
