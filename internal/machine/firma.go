package machine

// Snapshots firmados.
//
// Los sha256 de la integridad (verifyIntegrity) detectan CORRUPCIÓN: un bit que
// se dio la vuelta, una copia a medias. No detectan MANIPULACIÓN: quien pueda
// escribir en $KLING_ROOT cambia el overlay dorado y, acto seguido, el hash de
// meta.json, y la comprobación pasa. Tampoco impedían restaurar un snapshot
// traído de otro host, que con el TSC atado a la máquina acaba en un fallo
// críptico o en algo peor.
//
// La firma es un HMAC-SHA256 con una clave que solo existe en este host y solo
// lee root. Cubre lo que decide cómo nace una instancia: los hashes de los
// ficheros y la política (exec, red, dominios, volúmenes). NO cubre las
// anotaciones, que cambian después del commit por diseño y no deciden nada de
// eso.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// claveFirma es la clave del host. Se crea la primera vez, 32 bytes aleatorios,
// en $KLING_ROOT/secrets/snapshot.key con permisos 0600.
func (m *Manager) claveFirma() ([]byte, error) {
	m.firmaOnce.Do(func() {
		dir := filepath.Join(m.root, "secrets")
		ruta := filepath.Join(dir, "snapshot.key")
		if b, err := os.ReadFile(ruta); err == nil && len(b) >= 32 {
			m.firmaClave = b
			return
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			m.firmaErr = err
			return
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			m.firmaErr = err
			return
		}
		// O_EXCL: si dos daemons arrancan a la vez, que gane uno y el otro lea
		// la suya, en vez de firmar cada uno con una clave distinta.
		f, err := os.OpenFile(ruta, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if b2, rerr := os.ReadFile(ruta); rerr == nil && len(b2) >= 32 {
				m.firmaClave = b2
				return
			}
			m.firmaErr = err
			return
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			m.firmaErr = err
			return
		}
		if err := f.Close(); err != nil {
			m.firmaErr = err
			return
		}
		m.firmaClave = b
	})
	return m.firmaClave, m.firmaErr
}

// contenidoFirmado es lo que cubre la firma, en un orden fijo.
func contenidoFirmado(s *api.Snapshot) []byte {
	vols := make([]string, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		vols = append(vols, fmt.Sprintf("%s:%s:%v", v.Name, v.Mount, v.ReadOnly))
	}
	b, _ := json.Marshal(struct {
		Name, Image, Rootfs, Snap, Egress string
		Allow, Vols                       string
		AllowExec                         bool
		VCPUs, MemMiB                     int
	}{s.Name, s.Image, s.RootfsSHA256, s.SnapSHA256, s.Egress,
		strings.Join(s.AllowDomains, ","), strings.Join(vols, ","), s.AllowExec, s.VCPUs, s.MemMiB})
	return b
}

// firmar rellena s.Signature.
func (m *Manager) firmar(s *api.Snapshot) error {
	clave, err := m.claveFirma()
	if err != nil {
		return fmt.Errorf("snapshot signing key: %w", err)
	}
	mac := hmac.New(sha256.New, clave)
	mac.Write(contenidoFirmado(s))
	s.Signature = hex.EncodeToString(mac.Sum(nil))
	return nil
}

// errFirma es que la firma no cuadra.
var errFirma = errors.New("snapshot signature does not match")

// comprobarFirma exige que el snapshot lo firmara este host. Uno sin firma es
// anterior a esto: se acepta, salvo con KLING_REQUIRE_SIGNED=1.
func (m *Manager) comprobarFirma(s *api.Snapshot) error {
	if s.Signature == "" {
		if os.Getenv("KLING_REQUIRE_SIGNED") == "1" {
			return fmt.Errorf("%w: snapshot %q is not signed and KLING_REQUIRE_SIGNED=1; recommit it", errFirma, s.Name)
		}
		return nil
	}
	clave, err := m.claveFirma()
	if err != nil {
		return fmt.Errorf("snapshot signing key: %w", err)
	}
	mac := hmac.New(sha256.New, clave)
	mac.Write(contenidoFirmado(s))
	esperada := mac.Sum(nil)
	got, err := hex.DecodeString(s.Signature)
	if err != nil || !hmac.Equal(got, esperada) {
		return fmt.Errorf("%w: %q was modified outside kindling, or was made on another host. "+
			"Snapshots are bound to the host that made them; recommit it here", errFirma, s.Name)
	}
	return nil
}
