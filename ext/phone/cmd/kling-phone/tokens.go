package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/juan52878911/kindling/pkg/api"
)

// Dónde vive el token de cada teléfono.
//
// En el store del daemon (ns "phone", clave el id de la máquina): lo lee quien
// alcanza el socket, la misma confianza que el proxy por el que viajan las
// llamadas, y así el muro, el servidor MCP y el CLI lo encuentran aunque
// corran en máquinas distintas.
//
// Con la autorización del daemon activa (docs/authz.md), /store es solo de
// admin: un inquilino no puede leerlo ni escribirlo (403). Entonces el token
// va a un fichero 0600 del usuario en la máquina del CLI
// ($XDG_CONFIG_HOME/kling/phone-tokens/<daemon>/<id>.json): el inquilino sigue
// operando sus teléfonos, solo desde donde los creó.

func isForbidden(err error) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

var reMachineID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// localTokenDir es el directorio local de tokens para este daemon.
func (a *app) localTokenDir() (string, error) {
	if a.tokenDir != "" {
		return a.tokenDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(a.host))
	return filepath.Join(base, "kling", "phone-tokens", hex.EncodeToString(h[:6])), nil
}

func (a *app) localTokenPath(id string) (string, error) {
	if !reMachineID.MatchString(id) {
		return "", fmt.Errorf("bad machine id %q", id)
	}
	d, err := a.localTokenDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".json"), nil
}

func (a *app) getToken(ctx context.Context, id string) (*tokenRec, error) {
	var r tokenRec
	err := a.d.GetStore(ctx, storeNS, id, &r)
	if err == nil {
		return &r, nil
	}
	if !isForbidden(err) && !api.IsNotFound(err) {
		return nil, err
	}
	p, perr := a.localTokenPath(id)
	if perr != nil {
		return nil, perr
	}
	b, rerr := os.ReadFile(p)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return nil, &api.StatusError{Code: http.StatusNotFound, Message: "no API token"}
		}
		return nil, rerr
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &r, nil
}

func (a *app) putToken(ctx context.Context, r *tokenRec) error {
	err := a.d.PutStore(ctx, storeNS, r.Machine, r)
	if err == nil || !isForbidden(err) {
		return err
	}
	p, err := a.localTokenPath(r.Machine)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// pruneTokens borra los tokens de máquinas que ya no existen (un `kling rm` o
// un `graph rm` a mano, que no pasan por kling phone). Sin /store (inquilino)
// no hace nada: sus tokens locales se borran con `kling phone rm`.
func (a *app) pruneTokens(ctx context.Context, ms []*api.Machine) {
	keys, err := a.d.StoreKeys(ctx, storeNS)
	if err != nil {
		return
	}
	alive := map[string]bool{}
	for _, m := range ms {
		alive[m.ID] = true
	}
	for _, k := range keys {
		if !alive[k] {
			_ = a.d.DeleteStore(ctx, storeNS, k)
		}
	}
}

func (a *app) delToken(ctx context.Context, id string) error {
	err := a.d.DeleteStore(ctx, storeNS, id)
	if err != nil && !isForbidden(err) && !api.IsNotFound(err) {
		return err
	}
	if p, perr := a.localTokenPath(id); perr == nil {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
	}
	return nil
}
