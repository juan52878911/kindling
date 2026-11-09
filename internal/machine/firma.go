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
// eso. Tampoco cubre KernelSHA256 (K2): es una comprobación de compatibilidad
// entre el snapshot y el vmlinux instalado, no una decisión de arranque, y
// cubrirla habría invalidado la firma de todo snapshot anterior a ese campo
// (ver el comentario de KernelSHA256 en pkg/api/types.go).

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

	"github.com/juan52878911/kindling/internal/durable"
	"github.com/juan52878911/kindling/pkg/api"
)

// claveFirma es la clave del host. Se crea la primera vez, 32 bytes aleatorios,
// en $KLING_ROOT/secrets/snapshot.key con permisos 0600.
//
// Se cachea la clave, no el fallo: antes un sync.Once guardaba también el
// error, y un disco lleno o un permiso al primer intento dejaba al daemon sin
// firmar ni verificar dorados hasta reiniciarlo.
//
// La clave llega al disco antes de publicarse (durable.Escribir a un
// temporal y un enlace duro al nombre definitivo): sin el fsync, un corte
// podía dejar snapshot.key vacío, y O_EXCL fallaba sobre él para siempre. El
// enlace hace lo que hacía O_EXCL: si dos daemons la crean a la vez, gana uno
// y el otro lee la suya. Un fichero de menos de 32 bytes no firmó nada nunca
// (no se acepta como clave): es una clave a medias y se rehace.
func (m *Manager) claveFirma() ([]byte, error) {
	m.firmaMu.Lock()
	defer m.firmaMu.Unlock()
	if m.firmaClave != nil {
		return m.firmaClave, nil
	}
	k, err := crearOLeerClave(filepath.Join(m.root, "secrets"))
	if err != nil {
		return nil, err
	}
	m.firmaClave = k
	return k, nil
}

// crearOLeerClave lee la clave de dir/snapshot.key o la crea.
func crearOLeerClave(dir string) ([]byte, error) {
	ruta := filepath.Join(dir, "snapshot.key")
	leer := func() ([]byte, bool) {
		b, err := os.ReadFile(ruta)
		return b, err == nil && len(b) >= 32
	}
	if b, ok := leer(); ok {
		return b, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Una a medias (ver arriba) se retira; si otro la está rehaciendo a la
	// vez, el enlace de abajo decide quién gana.
	if fi, err := os.Stat(ruta); err == nil && fi.Size() < 32 {
		_ = os.Remove(ruta)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	// Un temporal por clave (de su propio contenido): dos que crean a la vez
	// no se pisan el temporal, y el enlace publica justo los bytes que se
	// devuelven.
	tmp := filepath.Join(dir, "snapshot.key."+hex.EncodeToString(b[:8])+".tmp")
	if err := durable.Escribir(tmp, b, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, ruta); err != nil {
		if b2, ok := leer(); ok {
			return b2, nil
		}
		return nil, err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return b, nil
}

// contenidoFirmado es lo que cubre la firma, en un orden fijo.
//
// s.KernelSHA256 y s.GuestIPv6Off quedan fuera A PROPÓSITO (K2 y la nota del
// campo GuestIPv6Off): son diagnóstico de compatibilidad, no una decisión de
// arranque, y cubrirlos invalidaría la firma de todo snapshot anterior a esos
// campos. Ver la nota de arriba y el comentario de cada campo en
// pkg/api/types.go.
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
		// omitempty: sin él (los dorados de antes de v2) firman lo mismo que
		// antes de existir el campo.
		RootfsDigest string `json:",omitempty"`
	}{s.Name, s.Image, s.RootfsSHA256, s.SnapSHA256, s.Egress,
		strings.Join(s.AllowDomains, ","), strings.Join(vols, ","), s.AllowExec, s.VCPUs, s.MemMiB,
		s.RootfsDigest})
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
