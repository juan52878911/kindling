package askllm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// EnvProvider elige el proveedor (anthropic u opencode); -provider manda sobre ella.
	EnvProvider = "KLING_DB_ASK_PROVIDER"
	// EnvFake es SOLO PARA PRUEBAS: un fichero cuyo contenido se devuelve como
	// respuesta del modelo, sin red. No salta ninguna comprobación: la SQL pasa
	// por sqlguard y por el rol de solo lectura igual que la de un modelo real.
	EnvFake = "KLING_DB_ASK_FAKE"

	maxFake = 64 << 10
)

// ErrNoProvider: ni clave de Anthropic ni opencode.
var ErrNoProvider = errors.New("no model provider available: set " + EnvKey +
	" (Anthropic API) or install opencode (https://opencode.ai)")

// Select elige el proveedor. name vacío: EnvProvider; sigue vacío: anthropic si
// hay clave, si no opencode si está instalado, si no ErrNoProvider. model vacío:
// el de cada proveedor. Con EnvFake puesto devuelve el falso de pruebas.
func Select(name, model string, timeout time.Duration) (Provider, error) {
	if f := os.Getenv(EnvFake); f != "" {
		return NewFake(f)
	}
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvProvider))
	}
	switch strings.ToLower(name) {
	case "anthropic":
		return FromEnv(model)
	case "opencode":
		bin, ok := FindOpenCode()
		if !ok {
			return nil, errors.New("opencode not found in PATH or ~/.opencode/bin")
		}
		return NewOpenCode(bin, model, timeout), nil
	case "":
		if strings.TrimSpace(os.Getenv(EnvKey)) != "" {
			return FromEnv(model)
		}
		if bin, ok := FindOpenCode(); ok {
			return NewOpenCode(bin, model, timeout), nil
		}
		return nil, ErrNoProvider
	}
	return nil, fmt.Errorf("unknown provider %q (anthropic or opencode)", name)
}

// Fake devuelve siempre el contenido de un fichero. Solo para pruebas.
type Fake struct{ text string }

// NewFake lee el fichero (hasta 64 KiB).
func NewFake(path string) (*Fake, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading "+EnvFake+": %w", err)
	}
	if len(b) > maxFake {
		return nil, errors.New(EnvFake + " file is too large")
	}
	return &Fake{text: string(b)}, nil
}

// Name implementa Provider.
func (*Fake) Name() string { return "a test file (" + EnvFake + ", nothing leaves this machine)" }

// Complete implementa Provider.
func (f *Fake) Complete(context.Context, string, string) (string, error) { return f.text, nil }
