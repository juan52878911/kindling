// Package imagen tiene lo que comparten los constructores de imágenes del
// núcleo escritos en Go (android, debian): armar una raíz de ficheros en
// memoria a partir de capas OCI y paquetes .deb fijados por sha256,
// escribirla como ext4 sin loop, chroot ni root (internal/ext4), meter el
// agente de invitado, el init y el /entrypoint, y, si se pide, poner la capa
// detrás de dm-verity (internal/verity) con la tabla en la base.
//
// Una imagen de estos constructores son dos ficheros, como las de
// 81-base-image.sh: la base (<base>.ext4: Debian con /sbin/overlay-init, que
// es minimal-init.sh) y la capa (<nombre>.layer.ext4: un ext4 cuyo /upper es
// lo que habría quedado en el upperdir de un overlay sobre la base). Con
// verity, el árbol de hashes y el FEC van pegados detrás de la capa y la
// tabla de device-mapper en la base, que el init lee antes de montar la capa.
package imagen

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/verity"
	"github.com/juan52878911/kindling/pkg/api"
)

// DebianBase es una base glibc fijada: una imagen OCI de Debian por digest
// más los .deb que se le añaden, cada uno con su sha256.
type DebianBase struct {
	Image, Index, Manifest string
	// Snapshot es la marca de snapshot.debian.org del día en que se fijó: si
	// deb.debian.org ya no tiene un paquete, se busca ahí; y los índices con
	// los que el constructor debian resuelve paquetes nuevos son los de ese
	// día, para que lo que añade cuadre con lo que ya hay en la base.
	Snapshot string
	Packages []DebPin
}

// DebPin es un .deb fijado. Es también una línea del lockfile del
// constructor debian (el "lock" de su spec).
type DebPin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"` // deb.debian.org o security.debian.org + Filename
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// ElfMachine es el e_machine de los binarios de cada arquitectura.
var ElfMachine = map[string]uint16{"arm64": 0xb7, "amd64": 0x3e}

// CheckELF comprueba que b es un ELF de esa arquitectura.
func CheckELF(b []byte, arch string) error {
	if len(b) < 20 || !bytes.Equal(b[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return fmt.Errorf("not an ELF binary")
	}
	if m := binary.LittleEndian.Uint16(b[18:]); m != ElfMachine[arch] {
		return fmt.Errorf("ELF machine %#x is not %s", m, arch)
	}
	return nil
}

// Marca es lo que distingue lo que deja cada constructor en la imagen: su
// nombre en los comentarios de lo generado, el directorio de la tabla de
// verity y el nombre del dispositivo de device-mapper.
type Marca struct {
	Constructor string // "android": "constructor android de kindling"
	Dir         string // "/etc/kindling-android": ahí va layer.verity
	DM          string // "android-layer": /dev/mapper/<DM>
}

func (m Marca) quien() string { return "constructor " + m.Constructor + " de kindling" }

// VerityConf es la ruta del fichero con la tabla de verity de la capa.
func (m Marca) VerityConf() string { return m.Dir + "/layer.verity" }

// UUID es el UUID (v4, derivado de la identidad de la construcción) del ext4
// kind ("base", "layer"...): mismas entradas, mismo UUID.
func UUID(tag, kind string, id [32]byte) [16]byte {
	s := sha256.Sum256(append([]byte(tag+"-uuid\x00"+kind+"\x00"), id[:]...))
	var u [16]byte
	copy(u[:], s[:16])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// Put cuelga un fichero regular con data en p y pone la hora al directorio.
func Put(root *ext4.Node, p string, data []byte, mode uint32, t time.Time) error {
	return PutNode(root, p, &ext4.Node{Mode: ext4.ModeReg | mode, Mtime: t, Size: int64(len(data)), Data: ext4.Bytes(data)}, t)
}

// PutNode cuelga n en p (con hora t si no la trae) y pone la hora al
// directorio, como haría el sistema de ficheros.
func PutNode(root *ext4.Node, p string, n *ext4.Node, t time.Time) error {
	if n.Mtime.IsZero() {
		n.Mtime = t
	}
	if err := root.Put(p, n, t); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	TouchParent(root, p, t)
	return nil
}

// Link pone el enlace simbólico p -> target (sin tocar la hora del padre).
func Link(root *ext4.Node, p, target string, t time.Time) error {
	if err := root.Put(p, &ext4.Node{Mode: ext4.ModeLink | 0o777, Target: target, Mtime: t}, t); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// TouchParent pone la hora del directorio donde se añadió algo, como haría
// el sistema de ficheros (install en un directorio le cambia la mtime).
func TouchParent(root *ext4.Node, p string, t time.Time) {
	if d, _ := root.Resolve(path.Dir(p)); d != nil && d.IsDir() {
		d.Mtime = t
	}
}

// PutAgent mete el agente de invitado en /usr/local/bin/kling-guest de root,
// comprobando antes que es un ELF de la arquitectura.
func PutAgent(root *ext4.Node, agent, arch string, t time.Time) error {
	b, err := os.ReadFile(agent)
	if err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	if err := CheckELF(b, arch); err != nil {
		return fmt.Errorf("guest agent %s: %w", agent, err)
	}
	return PutNode(root, "/usr/local/bin/kling-guest", &ext4.Node{Mode: ext4.ModeReg | 0o755, Size: int64(len(b)), Data: ext4.HostFile{Path: agent}}, t)
}

// SQ entrecomilla para sh.
func SQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// EnvPath guarda las variables de la receta, como en 81-base-image.sh: fuera
// del /entrypoint (0755, legible por todo el invitado), en un fichero 0600 de
// root que el entrypoint (PID 1, root) carga. Siguen en la capa, compartida
// por las máquinas de la imagen: para secretos, MMDS o credenciales.
const EnvPath = "/etc/kling/env"

// EnvFile son los `export` de env para EnvPath ("" si no hay ninguna).
func EnvFile(env []string) string {
	var e strings.Builder
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&e, "export %s=%s\n", k, SQ(v))
	}
	return e.String()
}

// Entrypoint es el /entrypoint de 81-base-image.sh: carga EnvPath, arranca
// service (si lo hay) en segundo plano y lo relanza si muere, y cede el PID 1
// al agente de invitado.
func Entrypoint(m Marca, env []string, service string) string {
	var e strings.Builder
	fmt.Fprintf(&e, "#!/bin/sh\n# Generado por el %s: el agente de invitado es PID 1.\n", m.quien())
	e.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\nexport HOME=/root\n")
	if len(env) > 0 {
		e.WriteString(". " + EnvPath + "\n")
	}
	if service != "" {
		fmt.Fprintf(&e, "( while :; do %s; echo \"service exited with $?, restarting in 1s\"; sleep 1; done ) </dev/null >>/var/log/service.log 2>&1 &\n", SQ(service))
	}
	e.WriteString("exec /usr/local/bin/kling-guest -listen :8080\n")
	return e.String()
}

// verityAnchor es la línea de minimal-init.sh que monta la capa.
const verityAnchor = `  mount -t ext4 -o ro "$LAYER_DEV" /overlay/svc`

// verityBlock es lo que se mete antes: la capa por /dev/mapper, y si dmsetup
// falla el init se para (montarla sin verificar sería el fallo de siempre).
// Los %[n]s son: 1 el prefijo de los mensajes, 2 quién la generó, 3 el
// fichero de la tabla y 4 el nombre del dispositivo.
const verityBlock = `  # %[1]s: dm-verity (%[2]s). La capa
  # se lee a través de dm-verity: un bloque que no cuadra con su hash da EIO
  # (y el kernel lo relee) en vez de entrar en la caché como bueno.
  if [ -f %[3]s ]; then
    tabla="$(grep -v '^#' %[3]s | sed "s#@DEV@#$LAYER_DEV#g")"
    if ! DM_DISABLE_UDEV=1 dmsetup create %[4]s --readonly --table "$tabla"; then
      echo "%[1]s: dm-verity on $LAYER_DEV failed; refusing to mount the layer unverified" >&2
      exit 1
    fi
    LAYER_DEV=/dev/mapper/%[4]s
  fi
`

// VerityInit mete en el init (minimal-init.sh) el bloque que monta la capa a
// través de dm-verity con la tabla de m.VerityConf().
func VerityInit(init string, m Marca) (string, error) {
	block := fmt.Sprintf(verityBlock, path.Base(m.Dir), m.quien(), m.VerityConf(), m.DM)
	lines := strings.Split(init, "\n")
	for i, l := range lines {
		if l == verityAnchor {
			out := append([]string{}, lines[:i]...)
			out = append(out, strings.Split(strings.TrimSuffix(block, "\n"), "\n")...)
			out = append(out, lines[i:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", fmt.Errorf("minimal-init.sh changed: can't find where the layer is mounted (%q)", verityAnchor)
}

// VerityFile es el contenido de m.VerityConf() con la tabla de la capa.
func VerityFile(m Marca, table string) []byte {
	return []byte("# dm-verity de la capa (" + m.quien() + "). Lo lee /sbin/overlay-init.\n" + table + "\n")
}

// Verity pega detrás de los dataBlocks primeros bloques del fichero p el
// árbol de dm-verity y fecRoots raíces de FEC (0 = sin FEC). La sal sale de
// la identidad de la construcción: mismas entradas, misma raíz.
func Verity(p string, dataBlocks uint64, tag string, id [32]byte, fecRoots int) (verity.Result, error) {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return verity.Result{}, err
	}
	salt := sha256.Sum256(append([]byte(tag+"-salt\x00"), id[:]...))
	res, err := verity.Append(f, dataBlocks, salt[:], fecRoots)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return verity.Result{}, fmt.Errorf("dm-verity: %w", err)
	}
	return res, nil
}

// VerityInfo apunta en built (lo que va a la receta) la raíz, la sal y la
// tabla de verity.
func VerityInfo(built map[string]any, res verity.Result, table string) {
	built["verity_root_hash"] = hex.EncodeToString(res.RootHash)
	built["verity_salt"] = hex.EncodeToString(res.Salt)
	built["verity_fec_roots"] = res.FECRoots
	built["verity_hash"] = "sha256"
	built["verity_table"] = table
}

// CheckBaseOwner falla si ya hay una base con ese nombre y no la escribió
// baseBuilder: el constructor solo sobrescribe sus propias bases.
func CheckBaseOwner(images, base, baseBuilder, constructor string) error {
	if _, err := os.Stat(filepath.Join(images, base+".ext4")); err != nil {
		return nil
	}
	var rec api.ImageRecipe
	rb, _ := os.ReadFile(filepath.Join(images, base+".recipe.json"))
	if json.Unmarshal(rb, &rec) != nil || rec.Builder != baseBuilder {
		return fmt.Errorf("base image %q already exists and was not written by the %s builder; choose another base_name", base, constructor)
	}
	return nil
}
