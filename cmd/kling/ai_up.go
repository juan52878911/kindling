package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// `kling ai up`: el gateway de IA con valores por defecto para probarlo ya.
//
// `ai serve` es lo que corre bajo systemd y por eso escucha por defecto en un
// socket Unix 0600, que curl no alcanza sin saber de sockets. `ai up` es la
// puerta de entrada de quien lo prueba por primera vez: busca el registro donde
// uno lo dejaría (el directorio actual o junto a config.json), escucha en
// loopback TCP y, antes de servir, dice la URL y un curl que funciona tal cual.
// Por debajo es `ai serve -listen`, así que el token es el mismo de siempre.

const aiUpListen = "127.0.0.1:8080"

func aiUp(args []string) error {
	fs := flag.NewFlagSet("ai up", flag.ExitOnError)
	host := hostFlag(fs)
	cfgFlag := fs.String("config", "", "model and task registry (default: ./ai.json, then "+aiDefault("ai.json")+")")
	listen := fs.String("listen", aiUpListen, "TCP address to listen on (always with a token)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; for other options use kling ai serve", fs.Arg(0))
	}
	cfg, err := aiUpConfig(*cfgFlag)
	if err != nil {
		return err
	}
	printAIUpBanner(os.Stdout, cfg, *listen, aiDefault("ai.token"), os.Getenv("KLING_AI_TOKEN") != "")

	serve := []string{"-config", cfg, "-listen", *listen}
	if *host != "" {
		serve = append(serve, "-H", *host)
	}
	return aiServe(serve)
}

// aiUpConfig resuelve el registro: el de -config si se dio; si no, ./ai.json
// y luego el que está junto a config.json. Sin ninguno el error dice dónde se
// buscó, que es lo primero que uno pregunta.
func aiUpConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	candidates := []string{"ai.json", aiDefault("ai.json")}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", errors.New("no ai.json in the current directory nor in " + aiDefault("ai.json") +
		"\nwrite the registry first (docs/ai-gateway.md has an example) or pass -config")
}

// aiUpURL es la URL con la que se llega a lo que escucha en listen. Una
// dirección sin host o comodín escucha en todas: se enseña la de loopback, que
// es la que sirve desde esta misma máquina.
func aiUpURL(listen string) string {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(h, p)
}

func printAIUpBanner(w io.Writer, cfg, listen, tokenFile string, envToken bool) {
	url := aiUpURL(listen)
	// El token nunca se imprime: el curl lo lee de donde está, igual que
	// haría cualquier cliente.
	auth := `"Authorization: Bearer $(cat ` + tokenFile + `)"`
	if envToken {
		auth = `"Authorization: Bearer $KLING_AI_TOKEN"`
	}
	fmt.Fprintf(w, "AI gateway: %s\n  registry: %s\n", url, cfg)
	fmt.Fprintf(w, "  try:  curl -s -H %s %s/v1/models\n", auth, url)
	fmt.Fprintf(w, "  health (no token):  curl -s %s/healthz\n\n", url)
}
