package upgrade

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Firma de releases (docs/firmas.md). El SHA256SUMS de cada release va firmado
// con ed25519 en el formato de minisign (SHA256SUMS.minisig), para que se pueda
// comprobar a mano con `minisign -V` y aquí con la stdlib. La clave privada
// vive en el secreto KLING_RELEASE_SIGNING_KEY del environment `release` de
// GitHub; las públicas, en clavesRelease. Formato (minisign.c):
//
//	clave pública  base64("Ed" | id[8] | pk[32])
//	firma          untrusted comment: …
//	               base64("ED" | id[8] | ed25519(BLAKE2b-512(datos)))   («Ed»: sin prehash)
//	               trusted comment: …
//	               base64(ed25519(firma[64] | comentario de confianza))
//	clave secreta  base64("Ed" | kdf "\0\0" | "B2" | sal[32] | ops[8] | mem[8] |
//	               id[8] | sk[64] | BLAKE2b-256("Ed"|id|sk))   (sin cifrar: minisign -W)

// FirmaObligatoriaDesde es la versión desde la que una release sin
// SHA256SUMS.minisig se rechaza. Cuenta la de la release que se baja y la del
// kling que la baja: un 1.x no acepta una release sin firmar, ni siquiera una
// anterior. Antes de ella, sin firma = solo sha256 y un aviso. Un -rc de esa
// versión ya cuenta como ella.
const FirmaObligatoriaDesde = "v1.0.0"

// clavesRelease son las claves públicas con las que se firman las releases,
// la línea base64 del .pub de minisign (scripts/firma-clave.sh la imprime).
// Más de una para rotar: se añade la nueva, se firma con ella y la vieja se
// quita una release después (docs/firmas.md §Rotar). Tiene que coincidir con
// KLING_RELEASE_PUBKEYS de scripts/install.sh (lo comprueba un test).
//
// Vacía hasta que Juan genere la clave real: hasta entonces una release
// firmada se acepta con aviso, y desde FirmaObligatoriaDesde no se acepta.
var clavesRelease = []string{}

// ClavesRelease devuelve las claves de clavesRelease ya leídas.
func ClavesRelease() []ClavePublica {
	out := make([]ClavePublica, 0, len(clavesRelease))
	for _, s := range clavesRelease {
		c, err := ParsearClavePublica(s)
		if err != nil {
			panic("upgrade: clave de release mal escrita: " + err.Error())
		}
		out = append(out, c)
	}
	return out
}

var (
	algEd = [2]byte{'E', 'd'} // ed25519 sobre los datos (el «legacy» de minisign)
	algED = [2]byte{'E', 'D'} // ed25519 sobre BLAKE2b-512 de los datos (el de siempre desde minisign 0.10)
	algB2 = [2]byte{'B', '2'}
)

// ErrClaveDesconocida: la firma es de una clave que no está entre las de confianza.
var ErrClaveDesconocida = errors.New("signed by a key that is not trusted")

const (
	prefijoNoConfianza = "untrusted comment: "
	prefijoConfianza   = "trusted comment: "
)

// ClavePublica es una clave de firma de releases: su id de 8 bytes (el que
// minisign imprime en hexadecimal) y la clave ed25519.
type ClavePublica struct {
	ID    [8]byte
	Clave ed25519.PublicKey
}

// IDHex es el id como lo escribe minisign: el uint64 little-endian en hex.
func (c ClavePublica) IDHex() string { return idHex(c.ID) }

func idHex(id [8]byte) string { return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(id[:])) }

// String es la línea base64 del .pub (la que va en clavesRelease y en `minisign -P`).
func (c ClavePublica) String() string {
	b := append(append(algEd[:], c.ID[:]...), c.Clave...)
	return base64.StdEncoding.EncodeToString(b)
}

// Fichero es el .pub de minisign entero.
func (c ClavePublica) Fichero() []byte {
	return []byte(prefijoNoConfianza + "minisign public key " + c.IDHex() + "\n" + c.String() + "\n")
}

// ParsearClavePublica lee la línea base64 de una clave pública o el .pub entero.
func ParsearClavePublica(s string) (ClavePublica, error) {
	linea := ""
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, prefijoNoConfianza) {
			continue
		}
		linea = l
		break
	}
	b, err := base64.StdEncoding.DecodeString(linea)
	if err != nil || len(b) != 2+8+ed25519.PublicKeySize {
		return ClavePublica{}, errors.New("not a minisign public key")
	}
	if [2]byte(b[:2]) != algEd {
		return ClavePublica{}, fmt.Errorf("unsupported public key algorithm %q", b[:2])
	}
	var c ClavePublica
	copy(c.ID[:], b[2:10])
	c.Clave = ed25519.PublicKey(bytes.Clone(b[10:]))
	return c, nil
}

// firmaMinisign es un .minisig ya leído.
type firmaMinisign struct {
	alg        [2]byte
	id         [8]byte
	sig        []byte
	comentario string // el de confianza, sin el prefijo
	global     []byte
}

func parsearFirma(b []byte) (*firmaMinisign, error) {
	lineas := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lineas) < 4 || !strings.HasPrefix(lineas[0], prefijoNoConfianza) || !strings.HasPrefix(lineas[2], prefijoConfianza) {
		return nil, errors.New("not a minisign signature")
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lineas[1]))
	if err != nil || len(s) != 2+8+ed25519.SignatureSize {
		return nil, errors.New("malformed minisign signature")
	}
	g, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lineas[3]))
	if err != nil || len(g) != ed25519.SignatureSize {
		return nil, errors.New("malformed minisign global signature")
	}
	f := &firmaMinisign{alg: [2]byte(s[:2]), sig: s[10:], global: g,
		comentario: strings.TrimPrefix(lineas[2], prefijoConfianza)}
	copy(f.id[:], s[2:10])
	if f.alg != algEd && f.alg != algED {
		return nil, fmt.Errorf("unsupported signature algorithm %q", f.alg[:])
	}
	return f, nil
}

// VerificarFirma comprueba que firma (un .minisig) es de datos y de una de
// claves, comentario de confianza incluido, y lo devuelve.
func VerificarFirma(datos, firma []byte, claves []ClavePublica) (string, error) {
	f, err := parsearFirma(firma)
	if err != nil {
		return "", err
	}
	var clave ed25519.PublicKey
	for _, c := range claves {
		if subtle.ConstantTimeCompare(c.ID[:], f.id[:]) == 1 {
			clave = c.Clave
			break
		}
	}
	if clave == nil {
		return "", fmt.Errorf("%w (key id %s)", ErrClaveDesconocida, idHex(f.id))
	}
	msg := datos
	if f.alg == algED {
		msg = blake2b(datos, 64)
	}
	if !ed25519.Verify(clave, msg, f.sig) {
		return "", errors.New("signature does not match: the file was altered or signed by another key")
	}
	if !ed25519.Verify(clave, append(bytes.Clone(f.sig), f.comentario...), f.global) {
		return "", errors.New("the signature's trusted comment was altered")
	}
	return f.comentario, nil
}

// CampoComentario saca el valor de clave:valor de un comentario de confianza
// con campos separados por tabuladores (el de minisign: timestamp:…\tfile:…).
func CampoComentario(comentario, clave string) string {
	for _, c := range strings.Split(comentario, "\t") {
		if v, ok := strings.CutPrefix(c, clave+":"); ok {
			return v
		}
	}
	return ""
}

// ClavePrivada es la clave con la que firma el paso de release.yml.
type ClavePrivada struct {
	ID    [8]byte
	Clave ed25519.PrivateKey
}

// GenerarClave hace un par nuevo con el azar de r (crypto/rand.Reader).
func GenerarClave(r io.Reader) (ClavePrivada, error) {
	var k ClavePrivada
	if _, err := io.ReadFull(r, k.ID[:]); err != nil {
		return k, err
	}
	_, sk, err := ed25519.GenerateKey(r)
	if err != nil {
		return k, err
	}
	k.Clave = sk
	return k, nil
}

// Publica es la clave pública de k.
func (k ClavePrivada) Publica() ClavePublica {
	return ClavePublica{ID: k.ID, Clave: bytes.Clone(k.Clave.Public().(ed25519.PublicKey))}
}

func sumaClaveSecreta(id [8]byte, sk []byte) []byte {
	return blake2b(append(append(algEd[:], id[:]...), sk...), 32)
}

// Fichero es la clave secreta sin cifrar en el formato de `minisign -G -W`:
// la cifra quien la guarda (el secreto de GitHub, el gestor de contraseñas).
func (k ClavePrivada) Fichero() []byte {
	var b []byte
	b = append(b, algEd[:]...)
	b = append(b, 0, 0) // sin KDF
	b = append(b, algB2[:]...)
	b = append(b, make([]byte, 32+8+8)...) // sal, opslimit, memlimit: sin uso sin KDF
	b = append(b, k.ID[:]...)
	b = append(b, k.Clave...)
	b = append(b, sumaClaveSecreta(k.ID, k.Clave)...)
	return []byte(prefijoNoConfianza + "kindling release signing key " + idHex(k.ID) + " (minisign, unencrypted)\n" +
		base64.StdEncoding.EncodeToString(b) + "\n")
}

// ParsearClavePrivada lee una clave secreta de minisign sin cifrar (la de
// Fichero o la de `minisign -G -W`). Una cifrada (scrypt) no: el KDF no está
// en la stdlib.
func ParsearClavePrivada(b []byte) (ClavePrivada, error) {
	var k ClavePrivada
	linea := ""
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, prefijoNoConfianza) {
			linea = l
			break
		}
	}
	d, err := base64.StdEncoding.DecodeString(linea)
	if err != nil || len(d) != 2+2+2+32+8+8+8+64+32 {
		return k, errors.New("not a minisign secret key")
	}
	if [2]byte(d[:2]) != algEd || [2]byte(d[4:6]) != algB2 {
		return k, errors.New("unsupported minisign secret key algorithm")
	}
	if d[2] != 0 || d[3] != 0 {
		return k, errors.New("encrypted minisign secret key: make it with `minisign -G -W` or scripts/firma-clave.sh")
	}
	r := d[54:]
	copy(k.ID[:], r[:8])
	sk := r[8:72]
	if subtle.ConstantTimeCompare(sumaClaveSecreta(k.ID, sk), r[72:]) != 1 {
		return k, errors.New("minisign secret key checksum mismatch")
	}
	k.Clave = ed25519.NewKeyFromSeed(sk[:32])
	if !bytes.Equal(k.Clave, sk) {
		return k, errors.New("minisign secret key is inconsistent")
	}
	return k, nil
}

// Firmar firma datos con k (prehash, como minisign por defecto) y devuelve el
// .minisig. comentario es el de confianza: va firmado.
func Firmar(k ClavePrivada, datos []byte, comentario string) ([]byte, error) {
	return firmarCon(k, datos, comentario, true)
}

func firmarCon(k ClavePrivada, datos []byte, comentario string, prehash bool) ([]byte, error) {
	if strings.ContainsAny(comentario, "\r\n") {
		return nil, errors.New("trusted comment must be a single line")
	}
	alg, msg := algEd, datos
	if prehash {
		alg, msg = algED, blake2b(datos, 64)
	}
	sig := ed25519.Sign(k.Clave, msg)
	global := ed25519.Sign(k.Clave, append(bytes.Clone(sig), comentario...))
	cuerpo := append(append(alg[:], k.ID[:]...), sig...)
	return []byte(prefijoNoConfianza + "signature from kindling release key " + idHex(k.ID) + "\n" +
		base64.StdEncoding.EncodeToString(cuerpo) + "\n" +
		prefijoConfianza + comentario + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n"), nil
}

// versionAlMenos dice si la etiqueta vX.Y.Z[-pre] llega a min (también
// vX.Y.Z). El sufijo de prerelease no cuenta: v1.0.0-rc1 ya es v1.0.0 a
// efectos de exigir firma. Lo que no es una versión ("dev", "") no llega.
func versionAlMenos(etiqueta, min string) bool {
	a, okA := tripleVersion(etiqueta)
	b, okB := tripleVersion(min)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func tripleVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	partes := strings.Split(v, ".")
	if len(partes) != 3 {
		return out, false
	}
	for i, p := range partes {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// NombreFirma es el asset con la firma del SHA256SUMS de una release.
const NombreFirma = "SHA256SUMS.minisig"

// comprobarFirma decide si el SHA256SUMS sumas (de la release etiqueta o de
// Dir) se acepta según su firma:
//
//   - con firma: tiene que ser válida, de una clave de confianza y, si es de
//     una release, para esa etiqueta (tag: del comentario de confianza; si no,
//     un SHA256SUMS firmado de otra release valdría para colar sus binarios);
//   - sin firma: error si es obligatoria (firmaObligatoria), si no, aviso;
//   - con firma y sin claves en este binario: lo mismo que sin firma.
//
// Un Dir (-from-dir) no la exige nunca: son binarios que el usuario trae,
// a menudo compilados por él; si trae SHA256SUMS.minisig, se comprueba.
func (f *Fuente) comprobarFirma(ctx context.Context, etiqueta string, sumas []byte) error {
	firma, donde, err := f.leerFirma(ctx, etiqueta)
	if err != nil {
		return err
	}
	obligatoria := f.Dir == "" && f.firmaObligatoria(etiqueta)
	claves := f.claves
	if claves == nil {
		claves = ClavesRelease()
	}
	if firma == nil || len(claves) == 0 {
		motivo := donde + " has no " + NombreFirma
		if firma != nil {
			motivo = "this kling has no release signing key built in to check " + donde + "'s " + NombreFirma
		}
		if obligatoria {
			return fmt.Errorf("%s: releases are signed from %s on, refusing it (docs/firmas.md)", motivo, FirmaObligatoriaDesde)
		}
		cuando := fmt.Sprintf(" (releases must be signed from %s on, docs/firmas.md)", FirmaObligatoriaDesde)
		if f.Dir != "" {
			cuando = ""
		}
		fmt.Fprintf(f.avisos(), "warning: %s; checked against SHA256SUMS only%s\n", motivo, cuando)
		return nil
	}
	comentario, err := VerificarFirma(sumas, firma, claves)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", donde, NombreFirma, err)
	}
	if f.Dir == "" {
		if t := CampoComentario(comentario, "tag"); t != etiqueta {
			return fmt.Errorf("%s: %s is signed for release %q, not for %s: refusing it", donde, NombreFirma, t, etiqueta)
		}
	}
	return nil
}

// firmaObligatoria: la release o este kling llegan a FirmaObligatoriaDesde.
func (f *Fuente) firmaObligatoria(etiqueta string) bool {
	return versionAlMenos(etiqueta, FirmaObligatoriaDesde) || versionAlMenos(f.VersionPropia, FirmaObligatoriaDesde)
}

// leerFirma trae el SHA256SUMS.minisig de la release o de Dir; nil sin error
// si no lo tiene (un 404, o el fichero no existe). Cualquier otro fallo es un
// error y no «sin firma»: un corte de red no rebaja la comprobación.
func (f *Fuente) leerFirma(ctx context.Context, etiqueta string) ([]byte, string, error) {
	if f.Dir != "" {
		b, err := leerLimitado(filepath.Join(f.Dir, NombreFirma), maxFirma)
		if os.IsNotExist(err) {
			return nil, f.Dir, nil
		}
		return b, f.Dir, err
	}
	var buf bytes.Buffer
	_, err := f.descargar(ctx, f.urlAsset(etiqueta, NombreFirma), &buf, maxFirma)
	if errors.Is(err, errNoEsta) {
		return nil, etiqueta, nil
	}
	if err != nil {
		return nil, etiqueta, fmt.Errorf("%s: %v", NombreFirma, err)
	}
	return buf.Bytes(), etiqueta, nil
}

func (f *Fuente) avisos() io.Writer {
	if f.Avisos == nil {
		return os.Stderr
	}
	return f.Avisos
}
