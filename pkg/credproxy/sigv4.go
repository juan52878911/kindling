package credproxy

// Re-firma SigV4 (credencial Kind "sigv4").
//
// EL PROBLEMA: S3 y compatibles no aceptan una clave en una cabecera: la
// petición va firmada con HMAC sobre su forma canónica. El invitado firma con
// una credencial MARCADOR (Placeholder es su access key id; su secret key da
// igual, el proxy no la usa) y el proxy descarta esa firma y firma de nuevo
// con la real. La firma solo depende de la petición canónica y de la clave,
// así que no hace falta conocer el secreto del invitado.
//
// QUÉ SE FIRMA: lo que el proxy ENVÍA, no lo que llegó. Host es el del
// dominio de la credencial, la ruta se manda ya codificada como se firma, y
// las cabeceras firmadas son las que firmó el invitado que sigan presentes
// tras quitar las de un solo salto, más host y x-amz-date. La región y el
// servicio son los de la credencial: el alcance (scope) que escribió el
// invitado se ignora, así que una regla acota a qué región y servicio se firma.
// x-amz-date se pone con el reloj del proxy (la deriva de la microVM no rompe
// la firma); en una URL prefirmada se respeta la fecha y la caducidad del
// invitado: el proxy no alarga lo que él pidió.
//
// CUERPO: con x-amz-content-sha256 en hex o UNSIGNED-PAYLOAD el cuerpo pasa
// sin tocar (el hash sigue valiendo: el cuerpo no cambia). Sin la cabecera se
// calcula el hash (hasta MaxSwapBody, en memoria). aws-chunked
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD, con o sin -TRAILER) lleva una firma por
// trozo encadenada con la anterior, empezando por la de la cabecera: se
// reescriben en flujo, un trozo en memoria cada vez. La firma mide 64 hex
// siempre, así que Content-Length no cambia. STREAMING-UNSIGNED-PAYLOAD-TRAILER
// no firma trozos: pasa tal cual. SigV4a (ECDSA) no se admite.
//
// QUÉ NO CAMBIA: quien puede mandar peticiones por el proxy puede hacer con la
// credencial lo que Allow deje; la firma nueva nunca vuelve al invitado, y la
// clave real solo entra en HMAC (ni en cabeceras ni en logs ni en auditoría).

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

const (
	// KindSigV4 es una credencial que firma con SigV4 en lugar de sustituir
	// un marcador: AccessKeyID y Secret son el par real, Placeholder el access
	// key id que usa el invitado.
	KindSigV4 = "sigv4"

	algoSigV4         = "AWS4-HMAC-SHA256"
	hashSinFirmar     = "UNSIGNED-PAYLOAD"
	hashTrozos        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	hashTrozosTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	hashSinFirmarTr   = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	formatoFecha      = "20060102T150405Z"
	// maxTrozoSigV4 es el trozo aws-chunked más grande que se retiene para
	// firmarlo (la firma va antes de los datos). Los clientes usan de 8 KiB a
	// 1 MiB.
	maxTrozoSigV4 = 4 << 20
)

var reNombreSigV4 = lazyre.New(`^[a-z0-9][a-z0-9-]{0,62}$`)
var reAccessKey = lazyre.New(`^[A-Za-z0-9]{8,128}$`)

// validarSigV4 comprueba una credencial Kind "sigv4" y pone el servicio por
// defecto ("s3").
func validarSigV4(c *Credential) error {
	d := c.Domain
	if !reAccessKey.MatchString(c.AccessKeyID) {
		return fmt.Errorf("credential for %s: the access key id must be 8-128 letters and digits", d)
	}
	if c.Service == "" {
		c.Service = "s3"
	}
	c.Region, c.Service = strings.ToLower(c.Region), strings.ToLower(c.Service)
	if !reNombreSigV4.MatchString(c.Region) {
		return fmt.Errorf("credential for %s: a sigv4 credential needs its region (us-east-1, auto…)", d)
	}
	if !reNombreSigV4.MatchString(c.Service) {
		return fmt.Errorf("credential for %s: invalid service %q", d, c.Service)
	}
	if c.Port != 0 || c.User != "" || c.Database != "" || c.AnyDatabase || c.CAPEM != "" ||
		c.Upstream != "" || c.UpstreamTLS != "" || c.TLSServerName != "" || c.UpstreamMachine != "" || c.UpstreamOwner != "" ||
		len(c.Headers) > 0 || c.Query || c.Body {
		return fmt.Errorf("credential for %s: a sigv4 credential takes only access key id, region, service and allow", d)
	}
	if err := ValidarPermisos(c.Allow); err != nil {
		return fmt.Errorf("credential for %s: %w", d, err)
	}
	return nil
}

// firmaInvitado es lo que se lee de la firma que puso el invitado.
type firmaInvitado struct {
	accessKey string
	// firmadas son las cabeceras firmadas, en minúsculas.
	firmadas []string
	// presignada: la firma va en la query (X-Amz-Signature), no en Authorization.
	presignada bool
	// fecha es X-Amz-Date de una URL prefirmada.
	fecha time.Time
	// firma es la que puso el invitado: sustituye a la emitida en los ecos.
	firma string
}

// leerFirmaInvitado extrae la firma SigV4 de r. (nil, nil) es que no lleva
// ninguna; error, que lleva una que el proxy no sabe re-firmar.
func leerFirmaInvitado(r *http.Request) (*firmaInvitado, error) {
	q := r.URL.Query()
	if auth := r.Header.Get("Authorization"); auth != "" {
		algo, resto, _ := strings.Cut(auth, " ")
		if !strings.HasPrefix(strings.ToUpper(algo), "AWS4-") {
			return nil, nil
		}
		if algo != algoSigV4 {
			return nil, errors.New("unsupported signature algorithm " + algo + " (only " + algoSigV4 + ")")
		}
		var f firmaInvitado
		for _, p := range strings.Split(resto, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			switch k {
			case "Credential":
				f.accessKey, _, _ = strings.Cut(v, "/")
			case "SignedHeaders":
				f.firmadas = strings.Split(strings.ToLower(v), ";")
			case "Signature":
				f.firma = v
			}
		}
		if f.accessKey == "" || len(f.firmadas) == 0 {
			return nil, errors.New("malformed Authorization header")
		}
		return &f, nil
	}
	if q.Get("X-Amz-Signature") == "" && q.Get("X-Amz-Credential") == "" {
		return nil, nil
	}
	if a := q.Get("X-Amz-Algorithm"); a != algoSigV4 {
		return nil, errors.New("unsupported signature algorithm " + a)
	}
	f := firmaInvitado{presignada: true, firmadas: strings.Split(strings.ToLower(q.Get("X-Amz-SignedHeaders")), ";"), firma: q.Get("X-Amz-Signature")}
	f.accessKey, _, _ = strings.Cut(q.Get("X-Amz-Credential"), "/")
	t, err := time.Parse(formatoFecha, q.Get("X-Amz-Date"))
	if err != nil || f.accessKey == "" {
		return nil, errors.New("malformed presigned URL")
	}
	f.fecha = t
	return &f, nil
}

// firmador firma con las claves de una credencial sigv4.
type firmador struct {
	accessKey, secret, region, service string
}

func (f firmador) ambito(fecha time.Time) string {
	return fecha.UTC().Format("20060102") + "/" + f.region + "/" + f.service + "/aws4_request"
}

// clave deriva la clave de firma del día.
func (f firmador) clave(fecha time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+f.secret), fecha.UTC().Format("20060102"))
	k = hmacSHA256(k, f.region)
	k = hmacSHA256(k, f.service)
	return hmacSHA256(k, "aws4_request")
}

func hexSHA256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// hashVacio es el SHA-256 del texto vacío.
var hashVacio = hexSHA256(nil)

// codificarAWS codifica como pide SigV4: solo A-Za-z0-9-._~ van sin escapar
// (y "/" si barra).
func codificarAWS(s string, barra bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && barra:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// rutaFirmada devuelve la ruta tal como se manda al servidor (codificada una
// vez) y como se firma: igual en S3; en los demás servicios, normalizada y
// codificada dos veces.
func rutaFirmada(decodificada, servicio string) (envio, canonica string) {
	if decodificada == "" {
		decodificada = "/"
	}
	envio = codificarAWS(decodificada, true)
	if servicio == "s3" {
		return envio, envio
	}
	p := path.Clean(decodificada)
	if strings.HasSuffix(decodificada, "/") && p != "/" {
		p += "/"
	}
	return envio, codificarAWS(codificarAWS(p, true), true)
}

// parQuery es un par de la query ya decodificado.
type parQuery struct{ k, v string }

// paresQuery parte la query cruda en pares decodificados, sin los de omitir.
func paresQuery(raw string, omitir ...string) []parQuery {
	var out []parQuery
	for _, p := range strings.Split(raw, "&") {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		dk, err1 := url.QueryUnescape(k)
		dv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			dk, dv = k, v
		}
		saltar := false
		for _, o := range omitir {
			saltar = saltar || dk == o
		}
		if !saltar {
			out = append(out, parQuery{dk, dv})
		}
	}
	return out
}

// queryCanonica ordena y codifica los pares como pide SigV4. Devuelve
// también la query que se manda, que es la misma cadena (así lo firmado y lo
// enviado coinciden byte a byte).
func queryCanonica(ps []parQuery) string {
	cs := make([]string, len(ps))
	for i, p := range ps {
		cs[i] = codificarAWS(p.k, false) + "=" + codificarAWS(p.v, false)
	}
	sort.Strings(cs)
	return strings.Join(cs, "&")
}

func valorCabecera(vs []string) string {
	partes := make([]string, len(vs))
	for i, v := range vs {
		partes[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(partes, ",")
}

// peticionCanonica monta la forma canónica de SigV4. firmadas va ordenada y en
// minúsculas; host es el que se envía.
func peticionCanonica(metodo, rutaCan, queryCan string, h http.Header, host string, firmadas []string, hashCuerpo string) string {
	var b strings.Builder
	b.WriteString(metodo + "\n" + rutaCan + "\n" + queryCan + "\n")
	for _, n := range firmadas {
		v := host
		if n != "host" {
			v = valorCabecera(h.Values(n))
		}
		b.WriteString(n + ":" + v + "\n")
	}
	b.WriteString("\n" + strings.Join(firmadas, ";") + "\n" + hashCuerpo)
	return b.String()
}

func (f firmador) firmaDe(fecha time.Time, canonica string) string {
	sts := algoSigV4 + "\n" + fecha.UTC().Format(formatoFecha) + "\n" + f.ambito(fecha) + "\n" + hexSHA256([]byte(canonica))
	return hex.EncodeToString(hmacSHA256(f.clave(fecha), sts))
}

// cabecerasAFirmar es el juego de cabeceras firmadas: las del invitado que
// siguen en h, más host y x-amz-date (y el hash del cuerpo si va), sin
// duplicados y ordenadas.
func cabecerasAFirmar(invitado []string, h http.Header, extra ...string) []string {
	vistas := map[string]bool{"host": true, "x-amz-date": true}
	for _, n := range extra {
		vistas[n] = true
	}
	for _, n := range invitado {
		if n != "" && (len(h.Values(n)) > 0 || n == "host") {
			vistas[n] = true
		}
	}
	out := make([]string, 0, len(vistas))
	for n := range vistas {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resultadoFirma es lo que el llamador necesita tras firmar.
type resultadoFirma struct {
	// semilla es la firma de la petición, de donde arranca la cadena de trozos.
	semilla string
	fecha   time.Time
}

// firmarPeticion firma out (ya con su URL saliente, cabeceras y Host) con la
// credencial. ruta y query ya van en la forma que se manda. hash es el valor
// de x-amz-content-sha256 (o el hash del cuerpo). Cabecera Authorization, o
// X-Amz-Signature en la query si presig. Para URLs prefirmadas, fecha es la del
// invitado; si no, ahora.
func (f firmador) firmarPeticion(out *http.Request, invitado *firmaInvitado, hash string, ahora time.Time, ps []parQuery) (resultadoFirma, error) {
	envio, rutaCan := rutaFirmada(out.URL.Path, f.service)
	out.URL.RawPath = envio
	if invitado.presignada {
		fecha := invitado.fecha
		cred := f.accessKey + "/" + f.ambito(fecha)
		pq := make([]parQuery, 0, len(ps)+1)
		for _, p := range ps {
			switch p.k {
			case "X-Amz-Credential":
				p.v = cred
			case "X-Amz-Security-Token":
				continue
			}
			pq = append(pq, p)
		}
		firmadas := invitado.firmadas
		sort.Strings(firmadas)
		canon := peticionCanonica(out.Method, rutaCan, queryCanonica(pq), out.Header, out.Host, firmadas, hash)
		sig := f.firmaDe(fecha, canon)
		out.URL.RawQuery = queryCanonica(pq) + "&X-Amz-Signature=" + sig
		return resultadoFirma{semilla: sig, fecha: fecha}, nil
	}
	out.Header.Set("X-Amz-Date", ahora.UTC().Format(formatoFecha))
	var extra []string
	if hash != "" && (f.service == "s3" || out.Header.Get("X-Amz-Content-Sha256") != "") {
		out.Header.Set("X-Amz-Content-Sha256", hash)
		extra = append(extra, "x-amz-content-sha256")
	}
	firmadas := cabecerasAFirmar(invitado.firmadas, out.Header, extra...)
	qc := queryCanonica(ps)
	out.URL.RawQuery = qc
	canon := peticionCanonica(out.Method, rutaCan, qc, out.Header, out.Host, firmadas, hash)
	sig := f.firmaDe(ahora, canon)
	out.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algoSigV4, f.accessKey, f.ambito(ahora), strings.Join(firmadas, ";"), sig))
	return resultadoFirma{semilla: sig, fecha: ahora}, nil
}

// Re-firma de trozos aws-chunked ---------------------------------------------

// resignador lee un cuerpo aws-chunked del invitado y emite el mismo cuerpo
// con las firmas de trozo recalculadas. Cada trozo es "<tamaño en hex>;
// chunk-signature=<64 hex>\r\n<datos>\r\n"; el último es de tamaño 0 y, con
// trailer, va seguido de las líneas del trailer y de x-amz-trailer-signature.
type resignador struct {
	src     *bufio.Reader
	f       firmador
	fecha   time.Time
	prev    string
	trailer bool
	pend    []byte
	fin     bool
	err     error
}

func nuevoResignador(src io.Reader, f firmador, r resultadoFirma, trailer bool) *resignador {
	return &resignador{src: bufio.NewReaderSize(src, 16<<10), f: f, fecha: r.fecha, prev: r.semilla, trailer: trailer}
}

var errTrozo = errors.New("malformed aws-chunked body")

func (r *resignador) Read(p []byte) (int, error) {
	for len(r.pend) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.fin {
			return 0, io.EOF
		}
		r.err = r.siguiente()
	}
	n := copy(p, r.pend)
	r.pend = r.pend[n:]
	return n, nil
}

func (r *resignador) linea() (string, error) {
	l, err := r.src.ReadSlice('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(l) < 2 || l[len(l)-2] != '\r' {
		return "", errTrozo
	}
	return string(l[:len(l)-2]), nil
}

func (r *resignador) siguiente() error {
	cab, err := r.linea()
	if err != nil {
		return err
	}
	tam, ext, ok := strings.Cut(cab, ";chunk-signature=")
	if !ok || len(ext) != 64 {
		return errTrozo
	}
	n, err := strconv.ParseInt(tam, 16, 32)
	if err != nil || n < 0 || n > maxTrozoSigV4 {
		return errTrozo
	}
	datos := make([]byte, n)
	if _, err := io.ReadFull(r.src, datos); err != nil {
		return io.ErrUnexpectedEOF
	}
	var crlf [2]byte
	if n > 0 || !r.trailer {
		if _, err := io.ReadFull(r.src, crlf[:]); err != nil || string(crlf[:]) != "\r\n" {
			return errTrozo
		}
	}
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + r.fecha.UTC().Format(formatoFecha) + "\n" + r.f.ambito(r.fecha) +
		"\n" + r.prev + "\n" + hashVacio + "\n" + hexSHA256(datos)
	sig := hex.EncodeToString(hmacSHA256(r.f.clave(r.fecha), sts))
	r.prev = sig
	out := append([]byte(tam+";chunk-signature="+sig+"\r\n"), datos...)
	if n > 0 || !r.trailer {
		out = append(out, '\r', '\n')
	}
	if n == 0 {
		r.fin = true
		if r.trailer {
			t, err := r.trailerFirmado()
			if err != nil {
				return err
			}
			out = append(out, t...)
		} else if _, err := r.src.ReadByte(); err != io.EOF {
			return errTrozo
		}
	}
	r.pend = out
	return nil
}

// trailerFirmado lee las líneas del trailer y devuelve el trailer con su
// firma recalculada (x-amz-trailer-signature).
func (r *resignador) trailerFirmado() ([]byte, error) {
	var canon, out strings.Builder
	for {
		l, err := r.linea()
		if err != nil {
			return nil, err
		}
		if l == "" {
			return nil, errTrozo // faltaba x-amz-trailer-signature
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return nil, errTrozo
		}
		if strings.EqualFold(k, "x-amz-trailer-signature") {
			if len(strings.TrimSpace(v)) != 64 {
				return nil, errTrozo
			}
			if fin, err := r.linea(); err != nil || fin != "" {
				return nil, errTrozo
			}
			sts := "AWS4-HMAC-SHA256-TRAILER\n" + r.fecha.UTC().Format(formatoFecha) + "\n" + r.f.ambito(r.fecha) +
				"\n" + r.prev + "\n" + hexSHA256([]byte(canon.String()))
			sig := hex.EncodeToString(hmacSHA256(r.f.clave(r.fecha), sts))
			out.WriteString(k + ":" + sig + "\r\n\r\n")
			return []byte(out.String()), nil
		}
		canon.WriteString(strings.ToLower(k) + ":" + strings.TrimSpace(v) + "\n")
		out.WriteString(l + "\r\n")
	}
}

// hashEsTrozos dice si hash es una de las variantes aws-chunked que lleva
// firma por trozo, y si lleva trailer.
func hashEsTrozos(hash string) (firmado, trailer bool) {
	return hash == hashTrozos || hash == hashTrozosTrailer, hash == hashTrozosTrailer
}

// ahoraSigV4 es el reloj de la firma; variable para los tests.
var ahoraSigV4 = time.Now

// Integración con ServeHTTP ----------------------------------------------------

// errPeticionSigV4 es una petición que el proxy no sabe re-firmar (400).
type errPeticionSigV4 string

func (e errPeticionSigV4) Error() string { return string(e) }

// buscarSigV4 dice si r viene firmada con el marcador de alguna credencial
// sigv4 de cs y cuál. (nil, -1, nil) es que no: la petición sigue por el
// camino normal (con el marcador tal cual, que el servidor rechazará).
func buscarSigV4(r *http.Request, cs []Credential) (*firmaInvitado, int, error) {
	hay := false
	for _, c := range cs {
		hay = hay || c.Kind == KindSigV4
	}
	if !hay {
		return nil, -1, nil
	}
	f, err := leerFirmaInvitado(r)
	if err != nil {
		return nil, -1, errPeticionSigV4(err.Error())
	}
	if f == nil {
		return nil, -1, nil
	}
	for i, c := range cs {
		if c.Kind == KindSigV4 && c.Placeholder == f.accessKey {
			return f, i, nil
		}
	}
	return nil, -1, nil
}

func (c Credential) firmador() firmador {
	return firmador{accessKey: c.AccessKeyID, secret: c.Secret, region: c.Region, service: c.Service}
}

func hashHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			return false
		}
	}
	return true
}

// cuerpoSigV4 prepara el cuerpo saliente de una petición re-firmada y dice qué
// hash va a la firma. src es el cuerpo del invitado, ya acotado y vigilado.
// Con trozos aws-chunked devuelve el resignador (a rellenar con la semilla en
// cuanto se firme); si no, el cuerpo pasa tal cual.
func cuerpoSigV4(r *http.Request, src io.Reader, f *firmaInvitado, servicio string) (body io.Reader, length int64, hash string, rs *resignador, err error) {
	length = r.ContentLength
	if f.presignada {
		hash = r.URL.Query().Get("X-Amz-Content-Sha256")
	} else {
		hash = r.Header.Get("X-Amz-Content-Sha256")
	}
	if hash == "" && f.presignada && servicio == "s3" {
		hash = hashSinFirmar
	}
	switch {
	case hash == "":
		// Sin hash declarado: se calcula, con el cuerpo en memoria.
		b, err := io.ReadAll(io.LimitReader(src, MaxSwapBody+1))
		if err != nil {
			return nil, 0, "", nil, err
		}
		if len(b) > MaxSwapBody {
			return nil, 0, "", nil, &http.MaxBytesError{Limit: MaxSwapBody}
		}
		return strings.NewReader(string(b)), int64(len(b)), hexSHA256(b), nil, nil
	case hash == hashSinFirmar, hash == hashSinFirmarTr, hashHex(hash):
		return src, length, hash, nil, nil
	}
	if firmado, tr := hashEsTrozos(hash); firmado {
		if f.presignada {
			return nil, 0, "", nil, errPeticionSigV4("aws-chunked is not valid in a presigned URL")
		}
		rs = &resignador{src: bufio.NewReaderSize(src, 16<<10), trailer: tr}
		return rs, length, hash, rs, nil
	}
	return nil, 0, "", nil, errPeticionSigV4("unsupported x-amz-content-sha256 " + hash)
}

// descartaSigV4 dice qué cabeceras del invitado no se copian al firmar de
// nuevo: su firma, su token (marcador) y la fecha, que pone el proxy.
func descartaSigV4(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Authorization", "X-Amz-Date", "X-Amz-Security-Token":
		return true
	}
	return false
}
