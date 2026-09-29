package doctor

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/pgmini"
)

// Revisión de un Postgres cualquiera desde el host (-url). La contraseña sale
// de PGPASSWORD, nunca de la URL: una URL con contraseña se rechaza, porque
// acaba en el historial del shell y en los listados de procesos.
//
// Habla TLS (sslmode de la URL, por defecto verify-full: cadena y nombre contra
// las raíces del sistema más sslrootcert/-ca-file) y SCRAM, con channel binding
// (-PLUS) si el servidor lo ofrece; nunca contraseña en claro. Sin TLS solo se
// habla con loopback, salvo -insecure explícito, y el informe lo avisa. Un
// sslmode que degradaría en silencio (allow, prefer) se rechaza.

type pgURL struct {
	addr, host, user, db string
	loopback             bool
	sslmode              string // disable | require | verify-ca | verify-full
	rootCert             string // sslrootcert de la URL ("" ninguno, "system" = solo el sistema)
}

func parseURL(raw string) (*pgURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// El error de url.Parse repite la URL entera: no se enseña.
		return nil, errors.New("doctor: the URL does not parse")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("doctor: the URL must start with postgres:// or postgresql://")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("doctor: the URL needs a user (postgres://user@host:port/db)")
	}
	if _, has := u.User.Password(); has {
		return nil, errors.New("doctor: remove the password from the URL and pass it in PGPASSWORD")
	}
	q := u.Query()
	mode := q.Get("sslmode")
	switch mode {
	case "":
		mode = pgmini.TLSVerifyFull
	case "disable", pgmini.TLSRequire, pgmini.TLSVerifyCA, pgmini.TLSVerifyFull:
	default:
		return nil, fmt.Errorf("doctor: sslmode=%s is not supported (disable, require, verify-ca or verify-full; allow and prefer could silently fall back to cleartext)", safe(mode, 20))
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("doctor: the URL needs a host")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	p := &pgURL{addr: net.JoinHostPort(host, port), host: host, user: u.User.Username(), db: strings.TrimPrefix(u.Path, "/"),
		sslmode: mode, rootCert: q.Get("sslrootcert")}
	if p.db == "" {
		p.db = p.user
	}
	if ip := net.ParseIP(host); ip != nil {
		p.loopback = ip.IsLoopback()
	} else {
		p.loopback = host == "localhost"
	}
	return p, nil
}

// maxCAFile acota un fichero de raíces: no se lee cualquier cosa entera.
const maxCAFile = 1 << 20

// rootPool son las raíces del sistema más las del fichero (nil si no hay
// fichero: pgmini usa las del sistema). "system" en la URL = solo el sistema.
func rootPool(path string) (*x509.CertPool, error) {
	if path == "" || path == "system" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("doctor: CA file: %w", err)
	}
	defer f.Close()
	pem, err := io.ReadAll(io.LimitReader(f, maxCAFile+1))
	if err != nil {
		return nil, fmt.Errorf("doctor: CA file: %w", err)
	}
	if len(pem) > maxCAFile {
		return nil, errors.New("doctor: CA file too large")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("doctor: the CA file holds no PEM certificate")
	}
	return pool, nil
}

// planTLS decide el modo TLS de pgmini y las raíces, o rechaza la combinación.
func planTLS(p *pgURL, t Target) (mode string, roots *x509.CertPool, err error) {
	if p.sslmode == "disable" {
		if t.CAFile != "" || t.TLSServerName != "" || p.rootCert != "" {
			return "", nil, errors.New("doctor: sslmode=disable excludes a CA file and -tls-server-name")
		}
		if !p.loopback && !t.Insecure {
			return "", nil, errors.New("doctor: sslmode=disable sends the whole check unencrypted; only loopback is allowed without -insecure (use sslmode=verify-full, or -insecure to accept the risk)")
		}
		return "", nil, nil
	}
	if p.sslmode == pgmini.TLSRequire && !p.loopback && !t.Insecure {
		return "", nil, errors.New("doctor: sslmode=require encrypts but does not verify who answers; use verify-full (or verify-ca), or -insecure to accept the risk")
	}
	path := p.rootCert
	if t.CAFile != "" {
		path = t.CAFile
	}
	roots, err = rootPool(path)
	if err != nil {
		return "", nil, err
	}
	return p.sslmode, roots, nil
}

type connQuerier struct{ c *pgmini.Conn }

func (c *connQuerier) query(ctx context.Context, _ string, name, sql string) (string, error) {
	rows, err := c.c.Query(ctx, "/* doctor:"+name+" */ "+sql)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("%s: expected one value, got %d rows", name, len(rows))
	}
	return rows[0][0], nil
}

func runURL(ctx context.Context, t Target, r *report) error {
	p, err := parseURL(t.URL)
	if err != nil {
		return err
	}
	mode, roots, err := planTLS(p, t)
	if err != nil {
		return err
	}
	c, err := pgmini.Dial(ctx, pgmini.Config{
		Addr: p.addr, User: p.user, Database: p.db,
		Password:      os.Getenv("PGPASSWORD"),
		Timeout:       15 * time.Second,
		NoCleartext:   true,
		TLSMode:       mode,
		TLSServerName: t.TLSServerName,
		RootCAs:       roots,
	})
	if err != nil {
		hint := ""
		if mode != "" && strings.Contains(err.Error(), "does not accept TLS") {
			hint = " (a server without TLS: sslmode=disable, only on loopback)"
		}
		return fmt.Errorf("doctor: connect to %s as %s: %w%s", safe(p.addr, 80), safe(p.user, 64), err, hint)
	}
	defer c.Close()
	r.target = fmt.Sprintf("postgres://%s@%s/%s", safe(p.user, 64), safe(p.addr, 80), safe(p.db, 64))
	switch {
	case mode == "" && !p.loopback:
		r.add("DB041", Info, "", "-insecure: the doctor talked to the server without TLS: its queries and results crossed the network unencrypted (the password did not: SCRAM only)")
	case mode == pgmini.TLSRequire && !p.loopback:
		r.add("DB041", Info, "", "-insecure: TLS without verifying the server (sslmode=require): a man in the middle could read the queries and results")
	}
	env := &sqlEnv{remote: !p.loopback, who: "current_user", dbs: []string{p.db}}
	sqlChecks(ctx, &connQuerier{c: c}, env, r)
	return nil
}
