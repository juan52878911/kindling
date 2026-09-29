package main

// MySQL/MariaDB en kling db (#64): el mismo modelo que Postgres (una copia es
// una microVM instanciada de una plantilla con el servidor caliente; cada
// copia estrena contraseña y la clave solo vive en el host), con otro motor.
//
// Qué cambia:
//   - La plantilla la hace scripts/db-golden-mysql.sh (o db-golden.sh build
//     -engine mysql): MariaDB de Alpine con el datadir en el overlay, y la
//     etiqueta kling.db.engine=mysql, que heredan las copias.
//   - La rotación: la clave se genera aquí y al invitado va solo su hash de
//     mysql_native_password (ext/db/internal/mysqlpw), por stdin, al cliente
//     del superusuario root por el socket local (unix_socket: solo el root
//     del invitado). Se comprueba en la misma sesión que mysql.user guarda
//     exactamente ese hash con ese plugin.
//   - connect: puerto 3306, DSN mysql:// y -mysql (el cliente del host).
//   - doctor y audit tienen su versión (internal/doctor/mysql.go,
//     internal/dbaudit, que lee el registro de conexiones de server_audit).
//
// Qué NO cambia para MySQL en esta versión (y se rechaza con un error claro):
// attach/detach (el modelo A), role, rehearse, snapshot/undo, tenant-check,
// ask y clone, que hablan SQL de Postgres o dependen de su catálogo.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/mysqlpw"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const (
	// labelEngine es el motor de la copia: "mysql" o, sin etiqueta,
	// Postgres (las copias anteriores a #64 no la llevan).
	labelEngine = "kling.db.engine"

	enginePostgres = "postgres"
	engineMySQL    = "mysql"

	myPort = 3306
)

// myClient es el cliente del superusuario dentro de la copia: root por el
// socket local (unix_socket: solo el root del sistema del invitado), sin
// cabeceras, sin escapar (-r: la salida es JSON o un número) y parando en el
// primer error (modo batch). La orden es fija: lo variable va por stdin.
// Sirve mariadb o mysql, el que haya.
const myClient = `c=$(command -v mariadb || command -v mysql) && exec "$c" --protocol=socket -uroot -N -B -r`

// myPing dice si el servidor contesta (vale también un "access denied": el
// servidor está vivo).
const myPing = `c=$(command -v mariadb-admin || command -v mysqladmin) && exec "$c" --protocol=socket -uroot ping --silent`

// engineOf es el motor de unas etiquetas.
func engineOf(labels map[string]string) string {
	if labels[labelEngine] == engineMySQL {
		return engineMySQL
	}
	return enginePostgres
}

// enginePort es el puerto del servidor de un motor.
func enginePort(engine string) int {
	if engine == engineMySQL {
		return myPort
	}
	return pgPort
}

// requirePostgres rechaza las operaciones que esta versión solo sabe hacer
// sobre Postgres.
func requirePostgres(mc *api.Machine, cmd string) error {
	if engineOf(mc.Labels) != enginePostgres {
		return fmt.Errorf("kling db %s supports postgres copies only in this version; %s is %s (see docs/mysql.md)", cmd, mc.Name, engineOf(mc.Labels))
	}
	return nil
}

// myReservedUsers no pueden ser el usuario de la aplicación: son del sistema.
var myReservedUsers = map[string]bool{"root": true, "mysql": true, "mariadb_sys": true, "mysql_sys": true}

// waitMySQL espera a que el servidor de la copia acepte conexiones.
func (a *app) waitMySQL(ctx context.Context, id string) error {
	deadline := time.Now().Add(a.readyWait)
	for {
		_, err := a.k.Run(ctx, nil, "exec", "-timeout", "10s", id, "--", "sh", "-c", myPing)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mysql in %s does not accept connections after %s", shortID(id), a.readyWait)
		}
		a.sleep(time.Second)
	}
}

// rotateMySQL estrena la contraseña del usuario de la aplicación en la copia
// id. Como rotate: la clave se escribe en el host ANTES de tocar la base, y
// al invitado solo va su hash.
func (a *app) rotateMySQL(ctx context.Context, id, user string) error {
	pw, err := generatePassword()
	if err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	return a.setMySQLHash(ctx, id, user, mysqlpw.NativeHash(pw))
}

// setMySQLHash pone el hash h al usuario user@'%' de la copia id y comprueba
// en la misma sesión que mysql.user lo guarda con ese plugin.
func (a *app) setMySQLHash(ctx context.Context, id, user, h string) error {
	// user pasó identPattern y h es "*" y 40 hexadecimales: nada que escapar.
	if !identPattern.MatchString(user) || !mysqlpw.ValidHash(h) {
		return fmt.Errorf("rotating the password of %s: invalid user or hash", shortID(id))
	}
	sql := fmt.Sprintf("ALTER USER '%[1]s'@'%%' IDENTIFIED WITH %[3]s AS '%[2]s';\n"+
		"SELECT COUNT(*) FROM mysql.user WHERE User = '%[1]s' AND Host = '%%' AND plugin = '%[3]s' AND authentication_string = '%[2]s';\n",
		user, h, mysqlpw.NativePlugin)
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "60s", id, "--", "sh", "-c", myClient)
	if err != nil {
		// Sin el detalle: el error del cliente puede citar la sentencia.
		return fmt.Errorf("rotating the password of %s failed (output omitted: it may quote the statement)", shortID(id))
	}
	if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("rotating the password of %s: user %q does not hold the new hash", shortID(id), user)
	}
	return nil
}

// setPassword pone en la copia la contraseña pw (ya en el host) con el
// mecanismo de su motor: verificador SCRAM o hash de mysql_native_password.
// Lo usa kling db rotate, en los dos sentidos (la nueva y, si falla, la vieja).
func (a *app) setPassword(ctx context.Context, mc *api.Machine, role, pw string) error {
	if engineOf(mc.Labels) == engineMySQL {
		return a.setMySQLHash(ctx, mc.ID, role, mysqlpw.NativeHash(pw))
	}
	ver, err := newVerifier(pw)
	if err != nil {
		return err
	}
	return a.setVerifier(ctx, mc.ID, role, ver)
}

// runHostMySQL abre el cliente del host (mariadb o mysql) con la clave en
// MYSQL_PWD, nunca en argv. Sin TLS, como -psql con sslmode=disable: el tramo
// es el del host a su propio invitado. Cada cliente lo pide a su manera
// (mysqlClientTLSFlag).
func runHostMySQL(ctx context.Context, env, args []string) error {
	p, err := exec.LookPath("mariadb")
	if err != nil {
		if p, err = exec.LookPath("mysql"); err != nil {
			return errors.New("neither mariadb nor mysql is installed on this host: use kling db connect -dsn with your client")
		}
	}
	tls, err := mysqlClientTLSFlag(ctx, p, probeClient)
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, p, append([]string{tls}, args...)...)
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &plugin.ExitError{Code: ee.ExitCode()}
	}
	return err
}

// probeClient ejecuta el cliente con un solo argumento informativo (--version
// o --help) y devuelve lo que escribe. Sin stdin, sin la clave y con tiempo
// acotado.
func probeClient(ctx context.Context, p, arg string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, p, arg)
	c.Env = os.Environ()
	out, err := c.CombinedOutput()
	return string(out), err
}

// mysqlClientTLSFlag dice cómo apagar TLS en el cliente p: --skip-ssl en el de
// MariaDB (que no conoce --ssl-mode y lo rechaza) y --ssl-mode=DISABLED en el
// de MySQL (que en 8.x ya no acepta --skip-ssl). "mysql" puede ser
// cualquiera de los dos (en muchas distribuciones es el de MariaDB), así que
// se pregunta: primero --version y, si no lo aclara (o falla), --help, que
// lista las opciones de verdad. Si nada lo aclara no se adivina: un cliente
// con la opción equivocada no arranca, o arrancaría pidiendo TLS a una copia
// que no lo tiene.
func mysqlClientTLSFlag(ctx context.Context, p string, probe func(ctx context.Context, p, arg string) (string, error)) (string, error) {
	const mariadb, mysql = "--skip-ssl", "--ssl-mode=DISABLED"
	if filepath.Base(p) == "mariadb" {
		return mariadb, nil
	}
	v, verr := probe(ctx, p, "--version")
	if verr == nil && strings.Contains(v, "MariaDB") {
		return mariadb, nil
	}
	h, herr := probe(ctx, p, "--help")
	if herr == nil {
		switch {
		case strings.Contains(h, "MariaDB"):
			return mariadb, nil
		case strings.Contains(h, "--ssl-mode"):
			return mysql, nil
		case strings.Contains(h, "--skip-ssl"):
			return mariadb, nil
		}
	}
	why := verr
	if why == nil {
		why = herr
	}
	if why == nil {
		why = errors.New("neither --version nor --help names it")
	}
	return "", fmt.Errorf("can't tell whether %s is the MySQL or the MariaDB client (%v): use kling db connect -dsn with your client", p, why)
}
