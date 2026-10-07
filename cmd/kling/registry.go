package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
)

// kling registry: las credenciales de los registros privados, guardadas EN EL
// DAEMON (internal/daemon/registries.go), que es quien construye. La
// contraseña o el token se leen de stdin (sin eco si es una terminal), nunca
// de argv: argv lo ve cualquiera en ps y queda en el historial.

func cmdRegistry(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling registry [login|logout|ls|import]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "login":
		return registryLogin(rest)
	case "logout", "rm":
		return registryLogout(rest)
	case "ls", "list":
		return registryList(rest)
	case "import":
		return registryImport(rest)
	default:
		return fmt.Errorf("unknown subcommand %q: use login, logout, ls or import", sub)
	}
}

// errSinRegistros traduce el 404 de un daemon sin la capacidad.
func errSinRegistros(err error) error {
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == 404 && se.Message == "404 Not Found" {
		return &errWithHint{err: errors.New("this daemon doesn't keep registry credentials"),
			hint: "upgrade kindling on the host (the daemon needs the \"registry-auth\" capability)"}
	}
	return err
}

func registryLogin(args []string) error {
	fs := flag.NewFlagSet("registry login", flag.ExitOnError)
	host := hostFlag(fs)
	user := fs.String("u", "", "username (a token-only registry accepts any)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling registry login <registry> [-u user]   (the password or token is read from stdin)")
	}
	reg, err := oci.CredentialKey(fs.Arg(0))
	if err != nil {
		return err
	}
	tty := isTerminal(os.Stdin.Fd())
	if tty && *user == "" {
		fmt.Fprintf(os.Stderr, "Username for %s: ", reg)
		l, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && l == "" {
			return err
		}
		*user = strings.TrimSpace(l)
	}
	pass, err := leerSecreto(os.Stdin, tty, fmt.Sprintf("Password or token for %s: ", reg))
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	r, err := api.NewClient(hostOf(*host)).RegistryLogin(ctx, api.RegistryLoginRequest{Host: reg, Username: *user, Password: pass})
	if err != nil {
		return errSinRegistros(err)
	}
	fmt.Printf("%s  credentials saved on the daemon", r.Host)
	if r.Username != "" {
		fmt.Printf(" (user %s)", r.Username)
	}
	fmt.Println()
	fmt.Println("They're checked on the first import from it.")
	next("kling image import %s/<repo>:<tag>", r.Host)
	return nil
}

// leerSecreto lee la contraseña: en una terminal, una línea sin eco tras el
// aviso; si no, stdin entero (un pipe, un fichero) sin el salto final.
func leerSecreto(in *os.File, tty bool, aviso string) (string, error) {
	var b []byte
	if tty {
		fmt.Fprint(os.Stderr, aviso)
		restore, err := sinEco(in.Fd())
		if err != nil {
			return "", err
		}
		// Un Ctrl-C a medias no deja la terminal sin eco.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		hecho := make(chan struct{})
		go func() {
			select {
			case <-sig:
				restore()
				fmt.Fprintln(os.Stderr)
				os.Exit(130)
			case <-hecho:
			}
		}()
		l, rerr := bufio.NewReader(io.LimitReader(in, 64<<10)).ReadString('\n')
		close(hecho)
		signal.Stop(sig)
		restore()
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		b = []byte(l)
	} else {
		var err error
		if b, err = io.ReadAll(io.LimitReader(in, 64<<10+1)); err != nil {
			return "", err
		}
		if len(b) > 64<<10 {
			return "", errors.New("the password on stdin is over 64 KiB")
		}
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("empty password or token: pass it on stdin (echo \"$TOKEN\" | kling registry login <registry> -u <user>)")
	}
	return s, nil
}

func registryLogout(args []string) error {
	fs := flag.NewFlagSet("registry logout", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling registry logout <registry>...")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, a := range fs.Args() {
		reg, err := oci.CredentialKey(a)
		if err != nil {
			return err
		}
		if err := c.RegistryLogout(ctx, reg); err != nil {
			return errSinRegistros(err)
		}
		fmt.Printf("%s  credentials removed from the daemon\n", reg)
	}
	return nil
}

func registryList(args []string) error {
	fs := flag.NewFlagSet("registry ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	l, err := api.NewClient(hostOf(*host)).Registries(ctx)
	if err != nil {
		return errSinRegistros(err)
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(l)
	}
	if len(l) == 0 {
		fmt.Println("No registry credentials. Save some:  echo \"$TOKEN\" | kling registry login ghcr.io -u <user>")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REGISTRY\tUSER")
	for _, r := range l {
		u := r.Username
		if u == "" {
			u = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\n", r.Host, u)
	}
	return tw.Flush()
}

// dockerConfig es lo que se entiende de ~/.docker/config.json.
type dockerConfig struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// credencialesDocker saca de un config.json de Docker las credenciales que
// lleva en claro (auths.<registro>.auth, base64 de usuario:contraseña). Las de
// un almacén (credsStore, credHelpers) o con identitytoken no están ahí: las
// devuelve aparte, para decirlo. Con hosts, solo esos.
func credencialesDocker(b []byte, hosts []string) (map[string]oci.Credential, []string, error) {
	var cfg dockerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, nil, errors.New("not a valid Docker config.json")
	}
	quiero := map[string]bool{}
	for _, h := range hosts {
		k, err := oci.CredentialKey(h)
		if err != nil {
			return nil, nil, err
		}
		quiero[k] = true
	}
	out := map[string]oci.Credential{}
	var fuera []string
	for h, a := range cfg.Auths {
		k, err := oci.CredentialKey(h)
		if err != nil || (len(quiero) > 0 && !quiero[k]) {
			continue
		}
		c := oci.Credential{Username: a.Username, Password: a.Password}
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				fuera = append(fuera, k+" (its auth is not valid base64)")
				continue
			}
			u, p, ok := strings.Cut(string(raw), ":")
			if !ok {
				fuera = append(fuera, k+" (its auth is not user:password)")
				continue
			}
			c = oci.Credential{Username: u, Password: p}
		}
		if c.Password == "" {
			why := "no password in the file: kept by a credential helper"
			if a.IdentityToken != "" {
				why = "an identity token, not supported"
			}
			fuera = append(fuera, k+" ("+why+")")
			continue
		}
		out[k] = c
	}
	for h := range cfg.CredHelpers {
		if k, err := oci.CredentialKey(h); err == nil && (len(quiero) == 0 || quiero[k]) {
			if _, ok := out[k]; !ok {
				fuera = append(fuera, k+" (kept by credential helper "+cfg.CredHelpers[h]+")")
			}
		}
	}
	for k := range quiero {
		if _, ok := out[k]; !ok && !slicesHasPrefix(fuera, k+" ") {
			fuera = append(fuera, k+" (not in the file)")
		}
	}
	sort.Strings(fuera)
	return out, fuera, nil
}

func slicesHasPrefix(l []string, p string) bool {
	for _, s := range l {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func registryImport(args []string) error {
	fs := flag.NewFlagSet("registry import", flag.ExitOnError)
	host := hostFlag(fs)
	path := fs.String("config", "", "Docker config file (default ~/.docker/config.json, or $DOCKER_CONFIG/config.json)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *path == "" {
		dir := os.Getenv("DOCKER_CONFIG")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dir = filepath.Join(home, ".docker")
		}
		*path = filepath.Join(dir, "config.json")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	creds, fuera, err := credencialesDocker(b, fs.Args())
	if err != nil {
		return fmt.Errorf("%s: %w", *path, err)
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	if err := guardarCredenciales(ctx, api.NewClient(hostOf(*host)), creds); err != nil {
		return err
	}
	for _, f := range fuera {
		fmt.Printf("skipped %s\n", f)
	}
	if len(creds) == 0 {
		return &errWithHint{err: fmt.Errorf("no credentials to import from %s", *path),
			hint: "echo \"$TOKEN\" | kling registry login <registry> -u <user>"}
	}
	return nil
}

func guardarCredenciales(ctx context.Context, c *api.Client, creds map[string]oci.Credential) error {
	hosts := make([]string, 0, len(creds))
	for h := range creds {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		cr := creds[h]
		r, err := c.RegistryLogin(ctx, api.RegistryLoginRequest{Host: h, Username: cr.Username, Password: cr.Password})
		if err != nil {
			return fmt.Errorf("%s: %w", h, errSinRegistros(err))
		}
		fmt.Printf("%s  credentials saved on the daemon (user %s)\n", r.Host, r.Username)
	}
	return nil
}
