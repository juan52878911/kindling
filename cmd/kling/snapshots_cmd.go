package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// kling template ls|rm|inspect.
//
// "Plantilla" es el nombre público del snapshot dorado: lo que `save` crea y
// `run -from` instancia en milisegundos. `snapshots` y `rmi` siguen
// funcionando como alias (tree.go).

func cmdTemplate(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return snapshotsList(args)
	case "rm", "remove":
		return snapshotsRemove("template rm", args)
	case "inspect", "show":
		return snapshotsInspect(args)
	case "credential":
		return snapshotsCredential(args)
	}
	return fmt.Errorf("unknown subcommand %q: use ls, rm, inspect or credential", sub)
}

// snapshotsCredential es `kling template credential`: ata una clave a una
// plantilla para que cada instancia que nazca de ella (kling run -from, las
// réplicas del gateway MCP) la reciba en su proxy de credenciales al arrancar.
// Es el camino para un servicio MCP: nadie está delante para hacer `machine
// credential` a cada réplica. Como allí, la clave no viaja por la línea de
// comandos: -f o stdin.
func snapshotsCredential(args []string) error {
	fs := flag.NewFlagSet("template credential", flag.ExitOnError)
	host := hostFlag(fs)
	cf := credentialFlags(fs)
	clear := fs.Bool("clear", false, "remove every credential of the template")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || (!*clear && (*cf.domain == "" || *cf.env == "")) {
		return errors.New("usage: kling template credential <template> -domain api.example.com -env API_KEY [-allow-request 'GET /v1/balance']... [-f keyfile]  (reads stdin if no -f)\n" +
			"       kling template credential <template> -type postgres -domain db.example.com -user app (-database appdb | -any-database) [-port 5432] [-ca-file ca.pem] [-upstream host:port] [-upstream-tls verify-full|disable] [-tls-server-name N] -env PGPASSWORD [-f passfile]\n" +
			"       kling template credential <template> -type mysql -domain db.example.com -user app (-database appdb | -any-database) [-port 3306] [...same as postgres] -env MYSQL_PWD [-f passfile]\n" +
			"       kling template credential <template> -clear")
	}
	req := api.CredentialsRequest{Clear: *clear}
	var spec api.CredentialSpec
	if !*clear {
		var err error
		if spec, err = cf.spec(); err != nil {
			return err
		}
		req.Credentials = []api.CredentialSpec{spec}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).SetSnapshotCredentials(ctx, fs.Arg(0), req)
	if err != nil {
		return err
	}
	if *clear {
		fmt.Printf("%s  no credentials; new instances get none (running ones keep theirs)\n", s.Name)
		return nil
	}
	if spec.Type == credproxy.KindPostgres || spec.Type == credproxy.KindMySQL {
		fmt.Printf("%s  every new instance gets a placeholder in %s; the password only goes to %s through its proxy\n",
			s.Name, spec.Env, pgDestino(spec))
		fmt.Printf("      %s\n", pgConexion(spec))
		fmt.Printf("      running instances are not changed (kling machine credential does that)\n")
		return nil
	}
	fmt.Printf("%s  every new instance gets a placeholder in %s; the key only goes to https://%s through its proxy\n",
		s.Name, spec.Env, strings.ToLower(spec.Domain))
	fmt.Printf("      %s\n", describirAllow(spec.Allow))
	fmt.Printf("      %s\n", describirSitios(spec))
	fmt.Printf("      point the SDK at http://%s; running instances are not changed (kling machine credential does that)\n",
		strings.ToLower(spec.Domain))
	return nil
}

// credAvisos es a dónde van los avisos de las banderas de una credencial
// (stderr; los tests lo cambian).
var credAvisos io.Writer = os.Stderr

// credFlags son las banderas de una credencial, iguales en machine credential
// y template credential. La clave NUNCA va en una bandera: -f o stdin.
type credFlags struct {
	domain, env, file, typ, user, database, caFile *string
	anyDatabase, query, body                       *bool
	upstream, upstreamTLS, tlsServerName           *string
	port                                           *int
	allow, headers                                 *stringsFlag
}

func credentialFlags(fs *flag.FlagSet) *credFlags {
	c := &credFlags{allow: &stringsFlag{}, headers: &stringsFlag{}}
	c.domain = fs.String("domain", "", "the only host the key is sent to, e.g. api.stripe.com (postgres/mysql: the database server)")
	c.env = fs.String("env", "", "environment variable that receives the placeholder, e.g. STRIPE_API_KEY (postgres: PGPASSWORD, mysql: MYSQL_PWD)")
	c.file = fs.String("f", "", "file with the key or password (default: stdin)")
	c.typ = fs.String("type", "http", "http, or postgres or mysql for a database password")
	c.port = fs.Int("port", 0, "postgres/mysql: the server's port (default 5432 or 3306)")
	c.user = fs.String("user", "", "postgres/mysql: the role or user the password belongs to (the guest must connect as it)")
	c.database = fs.String("database", "", "postgres/mysql: the only database the guest may connect to (required unless -any-database; in mysql it is the default database, the GRANTs are the boundary)")
	c.anyDatabase = fs.Bool("any-database", false, "postgres/mysql: let the guest connect to any database the role has access to (instead of -database)")
	c.caFile = fs.String("ca-file", "", "postgres/mysql: PEM CA added to the system roots to verify the server")
	c.upstream = fs.String("upstream", "", "postgres/mysql: host:port the proxy connects to instead of the domain (loopback and LAN allowed, e.g. 127.0.0.1:5432 for a Docker database)")
	c.upstreamTLS = fs.String("upstream-tls", "", "postgres/mysql: verify-full (default) or disable (only with -upstream; queries travel unencrypted; postgres: SCRAM-SHA-256 only; mysql: only methods that do not send the password)")
	c.tlsServerName = fs.String("tls-server-name", "", "postgres/mysql: name the server certificate is verified against (default: -domain)")
	fs.Var(c.allow, "allow-request", allowRequestHelp)
	fs.Var(c.headers, "header", "http: also swap the placeholder in this request header (repeatable; Authorization and X-Api-Key always)")
	c.query = fs.Bool("query", false, "http: also swap the placeholder in the query string (?key=)")
	c.body = fs.Bool("body", false, "http: also swap the placeholder in the request body; UNSAFE if the provider can echo it back transformed (an LLM asked to repeat it in base64)")
	return c
}

// spec lee la clave (de -f o stdin) y la CA, y arma la credencial. Lo que
// no pega con el tipo se rechaza aquí, antes de leer la clave; el daemon lo
// valida todo igualmente.
func (c *credFlags) spec() (api.CredentialSpec, error) {
	s := api.CredentialSpec{Domain: *c.domain, Env: *c.env, Allow: *c.allow, Headers: *c.headers, Query: *c.query, Body: *c.body}
	switch *c.typ {
	case "", "http":
		if *c.port != 0 || *c.user != "" || *c.database != "" || *c.anyDatabase || *c.caFile != "" ||
			*c.upstream != "" || *c.upstreamTLS != "" || *c.tlsServerName != "" {
			return s, errors.New("-port, -user, -database, -any-database, -ca-file, -upstream, -upstream-tls and -tls-server-name are only for -type postgres or mysql")
		}
	case credproxy.KindPostgres, credproxy.KindMySQL:
		if len(*c.allow) > 0 {
			return s, errors.New("-allow-request is only for HTTP credentials")
		}
		if len(*c.headers) > 0 || *c.query || *c.body {
			return s, errors.New("-header, -query and -body are only for HTTP credentials")
		}
		if *c.user == "" {
			return s, fmt.Errorf("-type %s needs -user (the role the password belongs to)", *c.typ)
		}
		switch {
		case *c.database != "" && *c.anyDatabase:
			return s, errors.New("-database and -any-database are mutually exclusive")
		case *c.database == "" && !*c.anyDatabase:
			return s, fmt.Errorf("-type %s needs -database (the only database the guest may use), or -any-database to allow every database the role can connect to", *c.typ)
		}
		if *c.anyDatabase {
			if *c.typ == credproxy.KindMySQL {
				fmt.Fprintf(credAvisos, "warning: -any-database: the guest may connect to any database on %s that user %s has GRANTs on\n", *c.domain, *c.user)
			} else {
				fmt.Fprintf(credAvisos, "warning: -any-database: the guest may connect to any database on %s that role %s has CONNECT on\n", *c.domain, *c.user)
			}
		}
		if *c.typ == credproxy.KindMySQL && *c.database != "" {
			// En MySQL la base del arranque no es una frontera (USE otra).
			fmt.Fprintf(credAvisos, "note: in mysql -database is the database the guest starts in; it can still USE any database %s has GRANTs on\n", *c.user)
		}
		s.Type, s.Port, s.User, s.Database, s.AnyDatabase = *c.typ, *c.port, *c.user, *c.database, *c.anyDatabase
		s.Upstream, s.TLSServerName = *c.upstream, *c.tlsServerName
		switch *c.upstreamTLS {
		case "", credproxy.UpstreamTLSVerifyFull:
		case credproxy.UpstreamTLSDisable:
			if *c.upstream == "" {
				return s, errors.New("-upstream-tls disable needs -upstream (the address of your database)")
			}
			s.UpstreamTLS = credproxy.UpstreamTLSDisable
			switch {
			case *c.typ == credproxy.KindMySQL && !credproxy.UpstreamLoopback(*c.upstream):
				fmt.Fprintf(credAvisos, "warning: -upstream-tls disable: traffic to %s, including query data, is unencrypted, and mysql does not prove the server knows the password: whoever answers at that address gets the queries and a hash exchange (the password itself is not sent)\n", *c.upstream)
			case !credproxy.UpstreamLoopback(*c.upstream):
				fmt.Fprintf(credAvisos, "warning: -upstream-tls disable: traffic to %s, including query data, is unencrypted (the password is not: SCRAM-SHA-256 only)\n", *c.upstream)
			}
		default:
			return s, fmt.Errorf("unknown -upstream-tls %q (verify-full or disable)", *c.upstreamTLS)
		}
		if *c.caFile != "" {
			b, err := os.ReadFile(*c.caFile)
			if err != nil {
				return s, err
			}
			s.CAPEM = string(b)
		}
	default:
		return s, fmt.Errorf("unknown -type %q (http, postgres or mysql)", *c.typ)
	}
	secret, err := leerClave(*c.file)
	if err != nil {
		return s, err
	}
	s.Secret = secret
	return s, nil
}

// pgDestino dice a dónde y cómo sale la contraseña de una credencial
// Postgres o MySQL: "servidor:puerto over verified TLS", con el upstream fijado si lo
// hay.
func pgDestino(s api.CredentialSpec) string {
	modo := "over verified TLS"
	if s.TLSServerName != "" {
		modo = "over TLS verified as " + strings.ToLower(s.TLSServerName)
	}
	if s.UpstreamTLS == credproxy.UpstreamTLSDisable {
		modo = "without TLS (SCRAM-SHA-256 only: the password itself never crosses the wire)"
		if s.Type == credproxy.KindMySQL {
			modo = "without TLS (mysql_native_password or the caching_sha2_password fast path only: the password itself never crosses the wire)"
		}
	}
	if s.Upstream != "" {
		return fmt.Sprintf("%s (upstream %s) %s", strings.ToLower(s.Domain), s.Upstream, modo)
	}
	return pgServidor(s) + " " + modo
}

// pgServidor es "servidor:puerto" de una credencial Postgres sin upstream.
func pgServidor(s api.CredentialSpec) string {
	port := s.Port
	if port == 0 {
		port = credproxy.PGDefaultPort
		if s.Type == credproxy.KindMySQL {
			port = credproxy.MySQLDefaultPort
		}
	}
	return fmt.Sprintf("%s:%d", strings.ToLower(s.Domain), port)
}

// pgConexion dice cómo tiene que conectar el cliente del invitado.
func pgConexion(s api.CredentialSpec) string {
	db := ""
	if s.Database != "" {
		db = " dbname=" + s.Database
	}
	if s.Type == credproxy.KindMySQL {
		return fmt.Sprintf("connect with host=%s user=%s%s password=$%s (ssl-mode DISABLED or PREFERRED: the proxy handles TLS to the server)",
			strings.ToLower(s.Domain), s.User, strings.Replace(db, "dbname", "database", 1), s.Env)
	}
	return fmt.Sprintf("connect with host=%s user=%s%s password=$%s (sslmode disable or prefer, channel_binding not require: the proxy handles the server side)",
		strings.ToLower(s.Domain), s.User, db, s.Env)
}

// allowRequestHelp es la ayuda de -allow-request, igual en machine y template.
const allowRequestHelp = "only sign requests matching \"METHOD /path\" (repeatable; * is one path segment, a final /** any rest; default: every request)"

// describirAllow dice en una línea qué peticiones firmará el proxy con la
// clave. Se imprime siempre: rotar sin -allow-request quita las restricciones
// que hubiera, y eso no debe pasar en silencio.
func describirAllow(allow []string) string {
	if len(allow) == 0 {
		return "every request to that host gets the key (use -allow-request to restrict it)"
	}
	return "only these requests get the key, anything else is a 403: " + strings.Join(allow, ", ")
}

// describirSitios dice en una línea dónde cambiará el proxy el marcador.
func describirSitios(s api.CredentialSpec) string {
	sitios := "headers " + strings.Join(append(append([]string(nil), credproxy.CabecerasPorDefecto...), s.Headers...), ", ")
	if s.Query {
		sitios += ", the query"
	}
	if s.Body {
		sitios += " and the BODY (unsafe if the provider echoes what it gets)"
	}
	return "the placeholder is swapped only in " + sitios
}

// leerClave lee la clave de un fichero o de stdin, sin espacios alrededor.
func leerClave(file string) (string, error) {
	var raw []byte
	var err error
	if file != "" {
		raw, err = os.ReadFile(file)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	}
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", errors.New("the key is empty")
	}
	return secret, nil
}

func snapshotsList(args []string) error {
	fs := flag.NewFlagSet("template ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only names (for scripting)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	list, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	if *quiet {
		for _, s := range list {
			fmt.Fprintln(os.Stdout, s.Name)
		}
		return nil
	}
	return writeSnapshots(os.Stdout, list, *asJSON)
}

// writeSnapshots vive aparte de la llamada al daemon para poder probar la
// tabla y el JSON sin uno.
func writeSnapshots(w io.Writer, list []*api.Snapshot, asJSON bool) error {
	if asJSON {
		if list == nil {
			list = []*api.Snapshot{} // [] y no null: más fácil para jq
		}
		return json.NewEncoder(w).Encode(list)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tCPU/MEM\tMEMORY\tDISK\tINSTANCES\tAGE")
	// F2: un asterisco basta aquí; el detalle va en `kling template inspect`.
	// Evita una columna nueva para un caso que, con el tiempo, desaparece solo
	// (los dorados viejos se van recongelando).
	huboSinIPv6, huboObsoleto := false, false
	for _, s := range list {
		marca := ""
		if !s.GuestIPv6Off && !s.GuestIPv6Stack {
			marca = "*"
			huboSinIPv6 = true
		}
		// Obsoleto: no restaura con el VMM de ahora. Va delante del
		// asterisco porque es lo que impide usarlo.
		if s.Stale != "" {
			marca = "!" + marca
			huboObsoleto = true
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%d/%dMiB\t%s\t%s\t%d\t%s\n",
			s.Name, marca, s.Image, s.VCPUs, s.MemMiB,
			human(s.MemBytes), human(s.DiskBytes), s.Instances, since(s.CreatedAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if huboObsoleto || huboSinIPv6 {
		fmt.Fprintln(w)
	}
	if huboObsoleto {
		fmt.Fprintln(w, "! stale: made with another VMM version and can't be restored; re-save it (`kling template inspect <name>`)")
	}
	if huboSinIPv6 {
		fmt.Fprintln(w, "* frozen before the IPv6 barrier: `kling template inspect <name>` for details")
	}
	return nil
}

func snapshotsRemove(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	host := hostFlag(fs)
	force := fs.Bool("f", false, "do not ask for confirmation")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling %s <template>...", name)
	}
	if !*force && !confirmMany("template", fs.Args()) {
		return errAborted
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	for _, n := range fs.Args() {
		if err := c.RemoveSnapshot(ctx, n); err != nil {
			return err
		}
		fmt.Println(n)
	}
	return nil
}

func snapshotsInspect(args []string) error {
	fs := flag.NewFlagSet("template inspect", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling template inspect <template> [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	s, err := api.NewClient(hostOf(*host)).Snapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return writeSnapshot(os.Stdout, s, *asJSON)
}

// writeSnapshot imprime un snapshot. Las anotaciones son datos opacos de las
// extensiones: se enseña la clave y un extracto, no se interpretan.
func writeSnapshot(w io.Writer, s *api.Snapshot, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	fmt.Fprintf(w, "name:        %s\n", s.Name)
	fmt.Fprintf(w, "image:       %s\n", s.Image)
	fmt.Fprintf(w, "created:     %s (%s ago)\n", s.CreatedAt.Local().Format("2006-01-02 15:04:05"), since(s.CreatedAt))
	mem := fmt.Sprintf("%d MiB", s.MemMiB)
	if s.MemMaxMiB > 0 {
		mem += fmt.Sprintf(" (ceiling %d MiB)", s.MemMaxMiB)
	}
	fmt.Fprintf(w, "cpus/mem:    %d / %s\n", s.VCPUs, mem)
	fmt.Fprintf(w, "on disk:     %s memory, %s total\n", human(s.MemBytes), human(s.DiskBytes))
	fmt.Fprintf(w, "instances:   %d\n", s.Instances)
	if hecho := origenDorado(s); hecho != "" {
		fmt.Fprintf(w, "made with:   %s\n", hecho)
	}
	if s.Stale != "" {
		fmt.Fprintf(w, "stale:       %s; it can't be restored. Re-save it: "+
			"kling save -replace <machine> %s (or kling mcp import %s -force)\n", s.Stale, s.Name, s.Name)
	}
	if s.GuestIPv6Stack {
		fmt.Fprintf(w, "guest ipv6:  stack kept by the image (recipe guest_ipv6_stack), no v6 addresses; "+
			"the host namespace blocks it\n")
	} else if !s.GuestIPv6Off {
		fmt.Fprintf(w, "guest ipv6:  not confirmed off (frozen before the IPv6 barrier); the host "+
			"namespace still blocks it, but recommit from a fresh boot to close it in the guest too\n")
	}
	for i, v := range s.VolumeSet() {
		label := "volumes:"
		if i > 0 {
			label = ""
		}
		ro := ""
		if v.ReadOnly {
			ro = " (ro)"
		}
		fmt.Fprintf(w, "%-13s%s -> %s%s\n", label, v.Name, v.Mount, ro)
	}
	if len(s.Annotations) > 0 {
		keys := make([]string, 0, len(s.Annotations))
		for k := range s.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "annotations:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s  %s\n", k, excerpt(s.Annotations[k], 60))
		}
	}
	fmt.Fprintf(w, "\ninstantiate with:  kling run -from %s\n", s.Name)
	return nil
}

// excerpt acorta un JSON a n caracteres para una sola línea.
func excerpt(raw json.RawMessage, n int) string {
	r := []rune(strings.Join(strings.Fields(string(raw)), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// origenDorado es con qué se hizo s ("firecracker 1.12.0, kling 0.18.0"), o
// "" en un dorado anterior a meta.json v1, que no lo guarda.
func origenDorado(s *api.Snapshot) string {
	var partes []string
	if s.VMM != "" {
		partes = append(partes, s.VMM)
	}
	if s.MacOS != "" {
		partes = append(partes, "macOS "+s.MacOS)
	}
	if s.KlingVersion != "" {
		partes = append(partes, "kling "+s.KlingVersion)
	}
	return strings.Join(partes, ", ")
}
