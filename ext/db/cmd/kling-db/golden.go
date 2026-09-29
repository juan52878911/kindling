package main

// `kling db golden` envuelve scripts/db-golden.sh en vez de reimplementarlo.
//
// Por qué: el script es el método ya probado en el lab (docs/db-golden.md y la
// Fase 1 del banco se hicieron con él), lo están ajustando otros cambios (la
// auditoría le añade log_connections) y es una operación de administración
// rara, que se hace desde un checkout de kindling. Una copia en Go serían ~200
// líneas que tendrían que seguirle el paso para siempre. Lo que sí hace este
// envoltorio: encontrar el script sin ejecutar nada del directorio actual por
// sorpresa, y pasarle el mismo kling y el mismo daemon que usa `kling db`.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const goldenScriptEnv = "KLING_DB_GOLDEN_SCRIPT"

func cmdGolden(args []string) error {
	fs := flag.NewFlagSet("db golden", flag.ContinueOnError)
	host := fs.String("H", "", "daemon endpoint (socket or ssh://user@host)")
	script := fs.String("script", "", "path of scripts/db-golden.sh (default: $"+goldenScriptEnv+", or installed next to kling-db)")
	// Los flags de este comando van ANTES del subcomando; lo de después es del
	// script tal cual (db-golden.sh build -seed-mb 20 nombre).
	if err := fs.Parse(args); err != nil {
		return &plugin.ExitError{Code: 2, Err: err}
	}
	rest := fs.Args()
	if len(rest) == 0 || (rest[0] != "image" && rest[0] != "build") {
		return usageErr("usage: kling db golden [-script P] [-H host] image | build [options] <name>\n" +
			"  build options: -template T | -migrations DIR  -seed FILE | -seed-mb N  -as-super  -role R  -database B\n" +
			"                 -image I  -mem M  -cpus N  -state DIR  -keep   (see docs/db-golden.md)")
	}
	rest, cleanup, err := expandTemplate(rest)
	if err != nil {
		return err
	}
	defer cleanup()
	return runGoldenScript(context.Background(), *script, *host, rest, os.Stdin, os.Stdout, os.Stderr)
}

// runGoldenScript ejecuta db-golden.sh con esos argumentos, el mismo kling y
// el mismo daemon que usa kling db. Lo comparten golden y clone.
func runGoldenScript(ctx context.Context, scriptFlag, host string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	path, err := findGoldenScript(scriptFlag)
	if err != nil {
		return err
	}
	bin, err := klingc.Resolve()
	if err != nil {
		return err
	}
	if strings.ContainsAny(bin, " \t\n") {
		// El script parte $KLING por espacios (admite banderas).
		return fmt.Errorf("the kling path %q has spaces; db-golden.sh cannot run it (set $KLING to a path without spaces)", bin)
	}
	c := exec.CommandContext(ctx, "bash", append([]string{path}, args...)...)
	// El script genera su propia clave y nunca necesita la de otra base: una
	// PGPASSWORD del entorno (la de producción en kling db clone) no debe llegar
	// a él ni a nada de lo que lance.
	c.Env = append(sinClavesPG(os.Environ()), "KLING="+bin)
	if host != "" {
		c.Env = append(c.Env, "KLING_HOST="+host)
	}
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	// Cancelar no mata el script de golpe: con SIGINT su trap retira la
	// máquina de preparación y los temporales.
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 2 * time.Minute
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &plugin.ExitError{Code: ee.ExitCode()}
	}
	return err
}

// findGoldenScript busca db-golden.sh: el flag, la variable, o junto al
// binario (bin/kling-db → share/kindling/db-golden.sh, o en su mismo
// directorio). A propósito NO busca en el directorio actual: ejecutar el
// scripts/ de un repositorio cualquiera por estar dentro sería ejecutar código
// ajeno sin pedirlo.
func findGoldenScript(flagPath string) (string, error) {
	if flagPath != "" {
		return checkScript(flagPath)
	}
	if p := os.Getenv(goldenScriptEnv); p != "" {
		return checkScript(p)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, p := range []string{
			filepath.Join(dir, "..", "share", "kindling", "db-golden.sh"),
			filepath.Join(dir, "db-golden.sh"),
		} {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, nil
			}
		}
	}
	return "", errors.New("db-golden.sh not found: from a kindling checkout run\n" +
		"  kling db golden -script scripts/db-golden.sh build ...   (or set $" + goldenScriptEnv + ")")
}

func checkScript(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("db-golden.sh: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", p)
	}
	return filepath.Abs(p)
}

// sinClavesPG quita del entorno lo que lleva una contraseña de Postgres.
func sinClavesPG(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "PGPASSWORD=") || strings.HasPrefix(kv, "PGPASSFILE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
