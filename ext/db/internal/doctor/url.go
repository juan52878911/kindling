package doctor

import (
	"context"
	"errors"
	"fmt"
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
// pgmini no habla TLS: solo SCRAM (nunca contraseña en claro) y, contra un
// servidor que no es loopback, el informe avisa de que la revisión viajó sin
// cifrar. Un sslmode que exige TLS se rechaza en vez de degradarlo en silencio.

type pgURL struct {
	addr, host, user, db string
	loopback             bool
}

func parseURL(raw string) (*pgURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// El error de url.Parse repite la URL entera: no se enseña.
		return nil, errors.New("doctor: the URL does not parse")
	}
	if u.Scheme == "mysql" || u.Scheme == "mariadb" {
		return nil, errors.New("doctor: -url checks postgres only in this version; a mysql copy is checked by name (kling db doctor <copy>)")
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
	switch m := u.Query().Get("sslmode"); m {
	case "", "disable", "allow", "prefer":
	default:
		return nil, fmt.Errorf("doctor: sslmode=%s needs TLS, which the doctor does not speak; use a loopback forward or sslmode=disable", safe(m, 20))
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("doctor: the URL needs a host")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	p := &pgURL{addr: net.JoinHostPort(host, port), host: host, user: u.User.Username(), db: strings.TrimPrefix(u.Path, "/")}
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

func runURL(ctx context.Context, raw string, r *report) error {
	p, err := parseURL(raw)
	if err != nil {
		return err
	}
	c, err := pgmini.Dial(ctx, pgmini.Config{
		Addr: p.addr, User: p.user, Database: p.db,
		Password:    os.Getenv("PGPASSWORD"),
		Timeout:     15 * time.Second,
		NoCleartext: true,
	})
	if err != nil {
		return fmt.Errorf("doctor: connect to %s as %s: %w", safe(p.addr, 80), safe(p.user, 64), err)
	}
	defer c.Close()
	r.target = fmt.Sprintf("postgres://%s@%s/%s", safe(p.user, 64), safe(p.addr, 80), safe(p.db, 64))
	if !p.loopback {
		r.add("DB041", Info, "", "the doctor talks to the server without TLS: its queries and results crossed the network unencrypted (the password did not: SCRAM only)")
	}
	env := &sqlEnv{remote: !p.loopback, who: "current_user", dbs: []string{p.db}}
	sqlChecks(ctx, &connQuerier{c: c}, env, r)
	return nil
}
