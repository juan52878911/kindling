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
// ficheros y la política (exec, red, dominios, volúmenes). Desde la v2 (B2)
// también las etiquetas (kling.ports decide qué puertos alcanza el host,
// kling.owner de quién es), los topes (mem_max_mib, cpu_pct), los nombres del
// entorno y el digest del mem.file: ver contenidoFirmadoV2. NO cubre las
// anotaciones, que cambian después del commit por diseño y no deciden nada de
// eso. Tampoco cubre KernelSHA256 (K2): es una comprobación de compatibilidad
// entre el snapshot y el vmlinux instalado, no una decisión de arranque, y
// cubrirla habría invalidado la firma de todo snapshot anterior a ese campo
// (ver el comentario de KernelSHA256 en pkg/api/types.go).

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/juan52878911/kindling/internal/digest"
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

// firmaV2 es el prefijo de las firmas v2. Una v1 es el hex a secas: no lleva
// ":", así que no hay forma de confundirlas.
const firmaV2 = "v2:"

// contenidoFirmadoV2 es lo que cubre la firma v2: todo lo de la v1 y, además,
// lo que la v1 dejaba fuera y sí decide cómo nace una instancia (B2):
//
//   - Labels: kling.ports dice a qué puertos del invitado deja llegar el
//     daemon, y kling.owner de quién es la plantilla.
//   - MemMaxMiB, CPUPct y CPUPctFixed: los topes con que nace cada copia.
//   - EnvKeys: qué variables hay que volver a pedir al arrancarla.
//   - MemDigest: el mem.file, que es la RAM (y el kernel) del invitado. Sin él
//     la firma cubría el estado de los vCPU pero no la memoria que restauran.
//   - ImageDigest y BaseDigest (meta.json v4): el contenido del disco de solo
//     lectura sobre el que se congeló, no solo su nombre (disco_base.go).
//
// CredentialDomains y CredentialAnyDatabase NO: no se guardan en meta.json
// (anotarCredencialesPlantilla los rellena al leer, desde el almacén sellado
// con AEAD y el nombre del dorado como dato asociado, que ya detecta un
// cambio), y se pueden poner después del commit (SetSnapshotCredentials):
// cubrirlos invalidaría la firma de una plantilla al darle credenciales.
//
// Los mapas se serializan con las claves en orden (encoding/json), así que
// las etiquetas firman lo mismo se lean como se lean. El campo V separa los
// dominios: ningún contenido v1 coincide con uno v2.
//
// Vacío y ausente firman igual: meta.json los guarda con omitempty, así que
// un mapa o una lista vacíos al hacer commit vuelven como nil al leerlo, y
// firmar {} y comprobar null rompía la firma del propio host. Pasaba con todo
// nodo de grafo, cuyas etiquetas son solo de grafo y el commit las quita
// (sinEtiquetasGrafo deja un mapa vacío). No debilita nada: el fichero no
// puede distinguir los dos casos, y ninguno de los dos decide algo distinto.
func contenidoFirmadoV2(s *api.Snapshot) []byte {
	labels, envKeys := s.Labels, s.EnvKeys
	if len(labels) == 0 {
		labels = nil
	}
	if len(envKeys) == 0 {
		envKeys = nil
	}
	b, _ := json.Marshal(struct {
		V           int
		V1          json.RawMessage
		Labels      map[string]string
		MemMaxMiB   int
		CPUPct      int
		CPUPctFixed bool
		EnvKeys     []string
		MemDigest   string
		// omitempty: los dorados de antes de meta.json v4 (y los de fork) no
		// los llevan y firman lo mismo que antes de existir. Uno que los
		// lleva no puede perderlos sin que deje de cuadrar.
		ImageDigest string `json:",omitempty"`
		BaseDigest  string `json:",omitempty"`
	}{2, contenidoFirmado(s), labels, s.MemMaxMiB, s.CPUPct, s.CPUPctFixed,
		envKeys, s.MemDigest, s.ImageDigest, s.BaseDigest})
	return b
}

// firmaV3 es el prefijo de las firmas v3 (meta.json v5).
const firmaV3 = "v3:"

// contenidoFirmadoV3 es lo que cubre la firma v3: todo lo de la v2 y lo que
// la v2 dejaba fuera y decide cómo nace una copia (M13):
//
//   - SinAgente: run -from se salta el resync. Fuera de la firma, quien
//     pudiera escribir meta.json lo apagaba con la firma intacta.
//   - ConAgente: lo contrario (S4): una copia sin resync no se entrega.
//   - ShareWait: cuántas carpetas vivas espera el agente antes de lanzar el
//     servicio, y por tanto cuántas acepta run -from.
//
// Con omitempty, como los digests de la v2: falso y cero firman lo mismo que
// ausentes, que es lo que meta.json guarda.
func contenidoFirmadoV3(s *api.Snapshot) []byte {
	b, _ := json.Marshal(struct {
		V         int
		V2        json.RawMessage
		SinAgente bool `json:",omitempty"`
		ConAgente bool `json:",omitempty"`
		ShareWait int  `json:",omitempty"`
	}{3, contenidoFirmadoV2(s), s.SinAgente, s.ConAgente, s.ShareWait})
	return b
}

// macDe calcula el HMAC de contenido con la clave del host.
func (m *Manager) macDe(contenido []byte) ([]byte, error) {
	clave, err := m.claveFirma()
	if err != nil {
		return nil, fmt.Errorf("snapshot signing key: %w", err)
	}
	mac := hmac.New(sha256.New, clave)
	mac.Write(contenido)
	return mac.Sum(nil), nil
}

// firmar rellena s.Signature, siempre en v3.
func (m *Manager) firmar(s *api.Snapshot) error {
	sum, err := m.macDe(contenidoFirmadoV3(s))
	if err != nil {
		return err
	}
	s.Signature = firmaV3 + hex.EncodeToString(sum)
	return nil
}

// comprobarMAC dice si firma (el hex, sin prefijo) es el HMAC de contenido.
func (m *Manager) comprobarMAC(nombre, firma string, contenido []byte) error {
	esperada, err := m.macDe(contenido)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(firma)
	if err != nil || !hmac.Equal(got, esperada) {
		return errFirmaNoCuadra(nombre)
	}
	return nil
}

// errFirma es que la firma no cuadra.
var errFirma = errors.New("snapshot signature does not match")

// firmaNoExigida es daemon.require_signed off (FijarFirmaExigida). El cero
// vale exigirla.
var firmaNoExigida atomic.Bool

// FijarFirmaExigida dice si un dorado sin firma, o con la v1 tras migrar, se
// puede restaurar (daemon.require_signed; por defecto sí se exige). El
// daemon lo llama al arrancar, antes de crear el manager.
func FijarFirmaExigida(exigir bool) { firmaNoExigida.Store(!exigir) }

// firmaExigida es la política efectiva: KLING_REQUIRE_SIGNED (1/0) manda
// sobre daemon.require_signed.
func firmaExigida() bool {
	switch os.Getenv("KLING_REQUIRE_SIGNED") {
	case "1", "on", "true":
		return true
	case "0", "off", "false":
		return false
	}
	return !firmaNoExigida.Load()
}

// marcaFirmasV2 es el fichero (junto a la clave) que dice que este host ya
// pasó sus dorados de la firma v1 a la v2 (MigrarFirmas). Desde entonces una
// v1 no se re-firma: un meta.json con ella es uno viejo que alguien volvió a
// poner (una copia de seguridad, un meta.json.v2.bak), y la v1 no cubre las
// etiquetas ni los topes que pudo cambiar.
const marcaFirmasV2 = "signatures-v2"

func (m *Manager) firmasMigradas() bool {
	_, err := os.Stat(filepath.Join(m.root, "secrets", marcaFirmasV2))
	return err == nil
}

// marcaFirmasV3 dice que este host ya pasó sus dorados a la firma v3, con
// los digests de su imagen calculados (MigrarFirmas). Desde entonces una v2
// se rechaza: es un meta.json viejo puesto de nuevo (un meta.json.v4.bak), y
// la v2 no cubre sin_agente ni share_wait, ni obliga a llevar image_digest.
const marcaFirmasV3 = "signatures-v3"

func (m *Manager) firmasV3Migradas() bool {
	_, err := os.Stat(filepath.Join(m.root, "secrets", marcaFirmasV3))
	return err == nil
}

// esFork dice si el dorado name es el temporal de un fork, que no lleva los
// digests del overlay, la memoria ni la imagen (ver la DECISIÓN en commit).
func (m *Manager) esFork(name string) bool {
	_, err := os.Stat(filepath.Join(m.snapDir(name), forkMarca))
	return err == nil
}

// comprobarFirma exige que el snapshot lo firmara este host, en v2 (A2 de la
// segunda revisión):
//
//   - Sin firma: es de antes de las firmas, de otro sitio, o alguien le quitó
//     la firma (y con ella los digests, para que verifyIntegrity lo diera por
//     antiguo). Se rechaza salvo con daemon.require_signed off.
//   - Firma v1 (hex sin prefijo, los dorados de antes de B2): no cubre las
//     etiquetas (kling.owner, kling.ports), los topes ni el mem.file. Si
//     este host todavía no migró sus dorados, este se re-firma ahora en v2
//     (migrarFirmaV1) si la v1 y sus digests cuadran; si ya los migró, se
//     rechaza (salvo con require_signed off, que la comprueba como siempre).
//   - Firma v2: tiene que cuadrar, y lleva al menos el digest del snap.file.
//
// Una v2 no se puede rebajar a v1: la v1 de ese contenido no la calculó
// nadie, y sin la clave no se puede calcular. Si migra, s queda con la firma
// nueva.
func (m *Manager) comprobarFirma(s *api.Snapshot) error {
	exigir := firmaExigida()
	if s.Signature == "" {
		if exigir {
			return fmt.Errorf("%w: template %q is not signed. It predates template signing, was copied from "+
				"another host, or its meta.json was edited. Recreate it on this host:\n%s\n"+
				"or, if you trust it as it is, accept unsigned templates with "+
				"`kling config set daemon.require_signed off` and restart the daemon", errFirma, s.Name, remedioDorado(s.Name))
		}
		return nil
	}
	if !strings.HasPrefix(s.Signature, firmaV3) {
		// Una v1 o una v2: se comprueba la que sea y, si este host aún no
		// migró sus dorados a la v3, se re-firma ahora (migrarFirma).
		if err := m.comprobarFirmaVieja(s); err != nil || !exigir {
			return err
		}
		v1 := !strings.HasPrefix(s.Signature, firmaV2)
		if v1 && m.firmasMigradas() {
			return fmt.Errorf("%w: template %q has an old v1 signature, which does not cover its labels and limits, "+
				"and this host already moved its templates to v2: its meta.json was put back from a copy. Recreate it:\n%s",
				errFirma, s.Name, remedioDorado(s.Name))
		}
		if !v1 && m.firmasV3Migradas() {
			return fmt.Errorf("%w: template %q has an old v2 signature, which does not cover how its copies resync, "+
				"and this host already moved its templates to v3: its meta.json was put back from a copy. Recreate it:\n%s",
				errFirma, s.Name, remedioDorado(s.Name))
		}
		nuevo, err := m.migrarFirma(s.Name, s.Signature)
		if err != nil {
			return err
		}
		s.Signature, s.MemDigest = nuevo.Signature, nuevo.MemDigest
		s.ImageDigest, s.BaseDigest = nuevo.ImageDigest, nuevo.BaseDigest
		return nil
	}
	if err := m.comprobarMAC(s.Name, strings.TrimPrefix(s.Signature, firmaV3), contenidoFirmadoV3(s)); err != nil {
		return err
	}
	// Todo dorado firmado lleva el digest del snap.file (los de fork no
	// llevan el del overlay ni el de la memoria, pero este sí). Sin él, la
	// firma cubriría un dorado que verifyIntegrity no comprueba.
	if s.SnapSHA256 == "" {
		return fmt.Errorf("%w: template %q is signed but has no state digest; recreate it:\n%s", errFirma, s.Name, remedioDorado(s.Name))
	}
	// El digest de la imagen no hace falta exigirlo aquí (M13): la v3 lo
	// cubre, y solo la firman commit (que lo calcula salvo en un fork) y
	// migrarFirma (que lo calcula si faltaba). Un meta con firma v2 o v1 que
	// alguien vuelva a poner se rechaza tras la migración (marcaFirmasV3).
	return nil
}

// comprobarFirmaVieja comprueba una firma v1 (hex a secas) o v2.
func (m *Manager) comprobarFirmaVieja(s *api.Snapshot) error {
	if strings.HasPrefix(s.Signature, firmaV2) {
		return m.comprobarMAC(s.Name, strings.TrimPrefix(s.Signature, firmaV2), contenidoFirmadoV2(s))
	}
	return m.comprobarFirmaV1(s)
}

func errFirmaNoCuadra(name string) error {
	return fmt.Errorf("%w: %q was modified outside kindling, or was made on another host. "+
		"Snapshots are bound to the host that made them; recommit it here", errFirma, name)
}

// comprobarFirmaV1 comprueba una firma v1 sobre contenidoFirmado.
func (m *Manager) comprobarFirmaV1(s *api.Snapshot) error {
	return m.comprobarMAC(s.Name, s.Signature, contenidoFirmado(s))
}

// errYaMigrada corta editMeta cuando otro ya re-firmó el dorado.
var errYaMigrada = errors.New("already signed in v3")

// migrarFirma re-firma en v3 el dorado name, que llevaba la firma v1 o v2
// firma. Solo si es de este host y está como se congeló: la firma cuadra y
// los digests que tiene también (verifyIntegrity). Los que la firma vieja no
// tenía se calculan ahora sobre lo que hay, salvo en un snapshot de fork, que
// no los lleva nunca: el del mem.file (lo que la v1 no cubría) y los de su
// imagen (image_digest y base_digest, que un dorado de meta.json v3 no
// tenía: M13). Devuelve el snapshot re-firmado.
func (m *Manager) migrarFirma(name, firma string) (*api.Snapshot, error) {
	s, err := m.loadSnapshot(name)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(s.Signature, firmaV3) {
		return s, nil
	}
	if s.Signature != firma {
		return nil, errFirmaNoCuadra(name)
	}
	if err := m.comprobarFirmaVieja(s); err != nil {
		return nil, err
	}
	dir := m.snapDir(name)
	if err := m.verifyIntegrity(s, dir); err != nil {
		return nil, err
	}
	memDigest, imgDigest, baseDigest := s.MemDigest, s.ImageDigest, s.BaseDigest
	if !m.esFork(name) {
		if memDigest == "" {
			if memDigest, err = digest.Tree(filepath.Join(dir, "mem.file")); err != nil {
				return nil, fmt.Errorf("template %q: digest of its memory dump: %w", name, err)
			}
		}
		if imgDigest == "" && s.Image != "" {
			if imgDigest, baseDigest, err = m.digestsDeImagen(s.Image); err != nil {
				return nil, fmt.Errorf("template %q: digest of its image %q: %w", name, s.Image, err)
			}
		}
	}
	nuevo, err := m.editMeta(name, func(s *api.Snapshot) (string, error) {
		if strings.HasPrefix(s.Signature, firmaV3) {
			return "", errYaMigrada
		}
		if s.Signature != firma {
			return "", errFirmaNoCuadra(name)
		}
		s.MemDigest, s.ImageDigest, s.BaseDigest = memDigest, imgDigest, baseDigest
		return "signature moved to v3", m.firmar(s)
	})
	if errors.Is(err, errYaMigrada) {
		return m.loadSnapshot(name)
	}
	if err != nil {
		return nil, err
	}
	log.Printf("template %s: signature moved to v3 (it now covers labels, limits, the memory dump, its image and how its copies resync)", name)
	return nuevo, nil
}

// MigrarFirmas pasa a la firma v3 todos los dorados de este host que llevan
// la v1 o la v2 y cuadran, y deja las marcas (marcaFirmasV2, marcaFirmasV3)
// para que desde entonces una v1 o una v2 se rechacen. Una vez por host: con la marca puesta no hace nada. Si
// alguno no se pudo leer o re-firmar por algo pasajero (un disco lleno), no
// deja la marca y se reintenta en el próximo arranque; los que no cuadran (firma o
// digests) se quedan como están y no se restauran. Los que no tienen firma
// tampoco se pueden re-firmar (no hay con qué saber que son de aquí): se
// avisa. El daemon lo llama al arrancar, en segundo plano.
func (m *Manager) MigrarFirmas(ctx context.Context) {
	if m.firmasV3Migradas() {
		return
	}
	metas, _ := filepath.Glob(filepath.Join(m.root, "snapshots", "*", "meta.json"))
	pendiente := false
	var sinFirma []string
	for _, p := range metas {
		if ctx.Err() != nil {
			return
		}
		name := filepath.Base(filepath.Dir(p))
		// Un meta que no se pudo LEER (un error de disco, un permiso) puede
		// ser una v1 que cuadra: con la marca puesta ya no se re-firmaría y
		// se rechazaría para siempre. Se deja sin marca y se reintenta.
		if _, err := os.ReadFile(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("warning: template %s: reading its meta.json to move its signature to v2: %v; will retry on the next start", name, err)
			pendiente = true
			continue
		}
		s, err := m.loadSnapshot(name)
		if err != nil {
			continue // ilegible o de un kling más nuevo: no se restaura de todos modos
		}
		switch {
		case s.Signature == "":
			sinFirma = append(sinFirma, name)
		case !strings.HasPrefix(s.Signature, firmaV3):
			if _, err := m.migrarFirma(name, s.Signature); err != nil {
				log.Printf("warning: template %s keeps its old signature and won't restore: %v", name, err)
				// Sin su imagen tampoco se restauraría: no espera a nada.
				if !errors.Is(err, errFirma) && !errors.Is(err, errCorrupto) && !errors.Is(err, ErrNoImage) {
					pendiente = true
				}
			}
		}
	}
	if len(sinFirma) > 0 && firmaExigida() {
		log.Printf("warning: %d template(s) are not signed and won't restore (daemon.require_signed is on): %s; "+
			"recreate them, or set daemon.require_signed off", len(sinFirma), strings.Join(sinFirma, ", "))
	}
	if pendiente {
		return
	}
	for _, marca := range []string{marcaFirmasV2, marcaFirmasV3} {
		if err := durable.Escribir(filepath.Join(m.root, "secrets", marca), []byte("1\n"), 0o600); err != nil {
			log.Printf("warning: marking the templates as migrated to signature %s: %v", strings.TrimPrefix(marca, "signatures-"), err)
			return
		}
	}
}
