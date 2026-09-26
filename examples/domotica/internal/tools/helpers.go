package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/config"
)

// Utilidades que antes compartía con el resto de cmd/kling. Una extensión es
// otro binario y no puede importar el paquete main del núcleo, así que las
// pocas que usa se copian aquí en vez de exportarlas desde el núcleo.

// reorderFor mueve los flags delante de los posicionales para que un flag escrito
// DESPUÉS de un argumento posicional no se ignore en silencio (el paquete `flag`
// de Go deja de parsear al primer no-flag). Es la versión correcta de reorder:
//   - Consulta el flagset para saber qué flags son booleanos (y por tanto NO se
//     llevan el siguiente argumento), en vez de una lista hardcodeada.
//   - Se detiene en `--`: todo lo que sigue es el comando del servidor y se deja
//     intacto, sin reordenar.
//
// Los flags SÍ deben estar definidos en fs antes de llamar aquí (lo están: se
// define todo y luego se parsea).
func reorderFor(fs *flag.FlagSet, args []string) []string {
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		bf, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && bf.IsBoolFlag()
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Fin de las opciones: el resto es el comando del servidor.
			out := append(flags, positional...)
			return append(out, args[i:]...)
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) &&
				(len(args[i+1]) == 0 || args[i+1][0] != '-') && !isBool(a) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// human formatea bytes de forma compacta.
func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dM", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dK", b>>10)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// hostFlag registra -H. Vacío por defecto para que hostOf aplique la misma
// precedencia que el núcleo: -H > $KLING_HOST > contexto activo > socket local.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("H", "", "daemon endpoint (socket or ssh://user@host)")
}

// hostOf resuelve a qué daemon hablar con pkg/config, que lee KLING_CONFIG: el
// núcleo nos lo pasa en el entorno y así vemos la misma configuración que él.
func hostOf(flagValue string) string {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (falling back to defaults)\n", err)
		cfg = &config.Config{}
	}
	return cfg.Host(flagValue)
}

// aiDefault es la ruta de un fichero del gateway de IA (ai.sock, ai.token),
// junto a config.json como en el núcleo.
func aiDefault(name string) string { return filepath.Join(filepath.Dir(config.Path()), name) }

// aiToken lee el token del gateway de $KLING_AI_TOKEN o del fichero. A
// diferencia del núcleo nunca lo crea: esta extensión solo es cliente.
func aiToken(path string) (string, error) {
	if t := os.Getenv("KLING_AI_TOKEN"); t != "" {
		return t, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (%v): chmod 600 it", path, st.Mode().Perm())
	}
	// Acotado: un token es corto y el fichero puede no ser lo que parece.
	b, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 16 {
		return "", errors.New(path + ": token too short")
	}
	return t, nil
}
