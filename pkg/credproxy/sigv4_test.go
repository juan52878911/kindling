package credproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests de la re-firma SigV4. Los vectores salen de botocore (la
// implementación de referencia de AWS) con una hora fija; el proveedor de los
// tests de extremo a extremo comprueba las firmas con un verificador propio,
// escrito aparte del firmador.

const (
	vecAK     = "AKIDEXAMPLE"
	vecSK     = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	svMarcAK  = "kling-cred-s3marcador0011223344"
	svRealAK  = "GK31c2f218a2e44f485b94239e"
	svRealSK  = "b892c0665f0ada8a4755dae98baa3b133590e11dae3bcc1f9d769d67f16c3835"
	svMarcSK  = "kling-cred-secretoinvitado"
	svRegion  = "garage"
	svHost    = "example.com"
	svBucket  = "/mi-cubo"
	svMaxTest = 8 << 10
)

var tVec = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

// peticionSaliente arma la petición que firmaría el proxy: out con URL, Host y
// cabeceras ya puestas.
func peticionSaliente(metodo, rawurl, host string, h map[string]string) *http.Request {
	u, _ := url.Parse(rawurl)
	out := &http.Request{Method: metodo, URL: u, Host: host, Header: http.Header{}}
	for k, v := range h {
		out.Header.Set(k, v)
	}
	return out
}

func TestSigV4VectoresBotocore(t *testing.T) {
	f := firmador{accessKey: vecAK, secret: vecSK, region: "us-east-1", service: "s3"}
	casos := []struct {
		nombre, metodo, url, hash, esperada string
		cabs                                map[string]string
		firmadas                            []string
		serv                                string
	}{
		{"s3 put con hash del cuerpo", "PUT",
			"https://s3.example.com/mi-cubo/dir%20uno/%C3%B1and%C3%BA%2Bx%21%2A%27%28%29.txt?uploadId=abc%2Fdef&partNumber=3&a=b%20c",
			"0b894166d3336435c800bea36ff21b29eaa801a52f584c006c49289a0dcf6e2f",
			"554b69d23824afe74c77bbedec16bcda139a1c6a8b2dcddf97c2ab04fdc1d13f",
			map[string]string{"Content-Type": "text/plain"},
			[]string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}, "s3"},
		{"s3 sin cuerpo (hash del vacío)", "GET", "https://s3.example.com/mi-cubo?list-type=2&prefix=a%20b",
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"96e148f19aaa952e71bc121b5933c1ee6f856e7f6f7743d68c68e12e0f7cb732",
			nil, []string{"host", "x-amz-content-sha256", "x-amz-date"}, "s3"},
		// Fuera de S3: ruta normalizada y codificada dos veces.
		{"servicio no s3", "POST", "https://svc.example.com/a b/../c//d?Param2=v2&Param1=v1",
			"48359503d307de73daf6abb7970583fa4cce5354952ca0c7faea7262a4b858b7",
			"90795ff0c3711b3e5b2d628a63163d8fe8f6716d451e569068236dc382685996",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			[]string{"content-type", "host", "x-amz-date"}, "service"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			f := f
			f.service = c.serv
			u, _ := url.Parse(c.url)
			cabs := map[string]string{}
			for k, v := range c.cabs {
				cabs[k] = v
			}
			if c.serv == "s3" {
				cabs["X-Amz-Content-Sha256"] = c.hash
			}
			out := peticionSaliente(c.metodo, c.url, u.Host, cabs)
			// Lo que hace ServeHTTP: la ruta decodificada y la query por pares.
			ps := paresQuery(u.RawQuery)
			if _, err := f.firmarPeticion(out, &firmaInvitado{accessKey: "x", firmadas: c.firmadas}, c.hash, tVec, ps); err != nil {
				t.Fatal(err)
			}
			want := "AWS4-HMAC-SHA256 Credential=" + vecAK + "/20150830/us-east-1/" + c.serv + "/aws4_request, SignedHeaders=" +
				strings.Join(c.firmadas, ";") + ", Signature=" + c.esperada
			if got := out.Header.Get("Authorization"); got != want {
				t.Fatalf("firma distinta de botocore:\n got  %s\n want %s", got, want)
			}
		})
	}
}

// Una URL prefirmada por botocore con la credencial real y la que firma el
// invitado con un marcador acaban con la misma firma.
func TestSigV4PrefirmadaVector(t *testing.T) {
	const quiero = "ecfbbe1a2e66c7345c9a49ded4656a4837f20532e02c668da0ad0647ce2fb923"
	invitado := "https://s3.example.com/mi-cubo/clave%20nueva.txt?versionId=1&X-Amz-Algorithm=AWS4-HMAC-SHA256&" +
		"X-Amz-Credential=" + url.QueryEscape(svMarcAK+"/20150830/us-east-1/s3/aws4_request") +
		"&X-Amz-Date=20150830T123600Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=" + strings.Repeat("0", 64)
	u, _ := url.Parse(invitado)
	r := &http.Request{URL: u, Header: http.Header{}}
	fi, err := leerFirmaInvitado(r)
	if err != nil || fi == nil || !fi.presignada || fi.accessKey != svMarcAK {
		t.Fatalf("firma del invitado mal leída: %+v %v", fi, err)
	}
	out := peticionSaliente("GET", invitado, "s3.example.com", nil)
	f := firmador{accessKey: vecAK, secret: vecSK, region: "us-east-1", service: "s3"}
	if _, err := f.firmarPeticion(out, fi, hashSinFirmar, time.Now(), paresQuery(u.RawQuery, "X-Amz-Signature")); err != nil {
		t.Fatal(err)
	}
	q := out.URL.Query()
	if q.Get("X-Amz-Signature") != quiero {
		t.Errorf("firma %s, botocore %s", q.Get("X-Amz-Signature"), quiero)
	}
	if !strings.HasPrefix(q.Get("X-Amz-Credential"), vecAK+"/") || strings.Contains(out.URL.RawQuery, svMarcAK) {
		t.Errorf("la credencial del invitado debía cambiar: %s", out.URL.RawQuery)
	}
}

// Ejemplo «Signature Calculations for the Authorization Header: Transferring
// Payload in Multiple Chunks» de la documentación de S3: 65 KiB de 'a' en
// trozos de 64 KiB, 1 KiB y 0.
func TestSigV4TrozosEjemploDeLaDocumentacion(t *testing.T) {
	f := firmador{accessKey: "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", region: "us-east-1", service: "s3"}
	fecha := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	const semilla = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	// Se comprueba también la firma de la cabecera (la semilla) del ejemplo.
	u, _ := url.Parse("https://s3.amazonaws.com/examplebucket/chunkObject.txt")
	out := peticionSaliente("PUT", u.String(), "s3.amazonaws.com", map[string]string{
		"Content-Encoding":             "aws-chunked",
		"X-Amz-Decoded-Content-Length": "66560",
		"X-Amz-Storage-Class":          "REDUCED_REDUNDANCY",
		"Content-Length":               "66824",
	})
	res, err := f.firmarPeticion(out, &firmaInvitado{accessKey: "x", firmadas: []string{"content-encoding", "content-length", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-decoded-content-length", "x-amz-storage-class"}},
		hashTrozos, fecha, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.semilla != semilla {
		t.Fatalf("firma semilla %s, la documentación dice %s", res.semilla, semilla)
	}
	datos := bytes.Repeat([]byte("a"), 66560)
	entrada := trozosInvitado(f, res, [][]byte{datos[:65536], datos[65536:]}, false, strings.Repeat("0", 64))
	rs := nuevoResignador(bytes.NewReader(entrada), f, res, false)
	salida, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	quiero := []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	}
	var got []string
	for _, l := range strings.Split(string(salida), "\r\n") {
		if _, s, ok := strings.Cut(l, ";chunk-signature="); ok {
			got = append(got, s)
		}
	}
	if strings.Join(got, ",") != strings.Join(quiero, ",") {
		t.Fatalf("firmas de trozo:\n got  %v\n want %v", got, quiero)
	}
	if len(salida) != len(entrada) {
		t.Errorf("la longitud cambió: %d -> %d (Content-Length ya no valdría)", len(entrada), len(salida))
	}
}

// trozosInvitado construye un cuerpo aws-chunked como lo haría un cliente, con
// las firmas de la credencial f del invitado (marcador) partiendo de res.
func trozosInvitado(f firmador, res resultadoFirma, trozos [][]byte, trailer bool, trailerSig string) []byte {
	var b bytes.Buffer
	prev := res.semilla
	firma := func(datos []byte) string {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + res.fecha.UTC().Format(formatoFecha) + "\n" + f.ambito(res.fecha) + "\n" + prev + "\n" + hashVacio + "\n" + hexSHA256(datos)
		prev = hex.EncodeToString(hmacSHA256(f.clave(res.fecha), sts))
		return prev
	}
	for _, d := range append(append([][]byte{}, trozos...), nil) {
		b.WriteString(strconv.FormatInt(int64(len(d)), 16) + ";chunk-signature=" + firma(d) + "\r\n")
		if len(d) > 0 || !trailer {
			b.Write(d)
			b.WriteString("\r\n")
		}
	}
	if trailer {
		b.WriteString("x-amz-checksum-crc32:AAAAAA==\r\nx-amz-trailer-signature:" + trailerSig + "\r\n\r\n")
	}
	return b.Bytes()
}

func TestSigV4TrozosMalFormadosFallan(t *testing.T) {
	f := firmador{accessKey: vecAK, secret: vecSK, region: "r", service: "s3"}
	res := resultadoFirma{semilla: strings.Repeat("a", 64), fecha: tVec}
	buenos := trozosInvitado(f, res, [][]byte{[]byte("hola")}, false, "")
	for nombre, cuerpo := range map[string][]byte{
		"sin firma":         []byte("4\r\nhola\r\n0\r\n\r\n"),
		"truncado":          buenos[:len(buenos)-10],
		"tamaño enorme":     []byte("ffffffff;chunk-signature=" + strings.Repeat("a", 64) + "\r\n"),
		"basura al final":   append(append([]byte{}, buenos...), 'x'),
		"sin CRLF del dato": bytes.Replace(buenos, []byte("hola\r\n"), []byte("holaXX"), 1),
	} {
		if _, err := io.ReadAll(nuevoResignador(bytes.NewReader(cuerpo), f, res, false)); err == nil {
			t.Errorf("%s: debía fallar", nombre)
		}
	}
}

// Extremo a extremo --------------------------------------------------------

// verificadorS3 es el «servidor»: recalcula la firma de lo que recibe con la
// clave real, a su manera (la ruta cruda tal cual, como hace Garage), y lee el
// cuerpo comprobando la cadena de trozos.
type verificadorS3 struct {
	mu      sync.Mutex
	vistas  []string // «MÉTODO ruta?query -> resultado»
	cuerpos map[string][]byte
}

func (v *verificadorS3) manejar(t *testing.T, w http.ResponseWriter, r *http.Request) {
	status, msg := v.comprobar(r)
	v.mu.Lock()
	v.vistas = append(v.vistas, r.Method+" "+r.URL.RequestURI()+" -> "+strconv.Itoa(status))
	v.mu.Unlock()
	if status != 200 {
		http.Error(w, msg, status)
		return
	}
	// Eco del access key id real y del secreto: el redactor tiene que
	// quitarlos si hace falta.
	w.Header().Set("X-Eco", r.Header.Get("Authorization"))
	io.WriteString(w, "ok")
}

func (v *verificadorS3) comprobar(r *http.Request) (int, string) {
	const algo = "AWS4-HMAC-SHA256"
	f := firmador{accessKey: svRealAK, secret: svRealSK, region: svRegion, service: "s3"}
	q := r.URL.Query()
	var firmadas []string
	var recibida, hash string
	var fecha time.Time
	if auth := r.Header.Get("Authorization"); auth != "" {
		partes := map[string]string{}
		for _, p := range strings.Split(strings.TrimPrefix(auth, algo+" "), ", ") {
			k, val, _ := strings.Cut(p, "=")
			partes[k] = val
		}
		if !strings.HasPrefix(partes["Credential"], svRealAK+"/") {
			return 403, "InvalidAccessKeyId"
		}
		firmadas = strings.Split(partes["SignedHeaders"], ";")
		recibida = partes["Signature"]
		fecha, _ = time.Parse(formatoFecha, r.Header.Get("X-Amz-Date"))
		hash = r.Header.Get("X-Amz-Content-Sha256")
		if time.Since(fecha) > 5*time.Minute || time.Since(fecha) < -5*time.Minute {
			return 403, "RequestTimeTooSkewed"
		}
	} else {
		if !strings.HasPrefix(q.Get("X-Amz-Credential"), svRealAK+"/") {
			return 403, "InvalidAccessKeyId"
		}
		firmadas = strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
		recibida = q.Get("X-Amz-Signature")
		fecha, _ = time.Parse(formatoFecha, q.Get("X-Amz-Date"))
		hash = hashSinFirmar
		exp, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
		if time.Now().After(fecha.Add(time.Duration(exp) * time.Second)) {
			return 403, "ExpiredToken"
		}
	}
	// query canónica: pares crudos, sin la firma, ordenados.
	var pares []string
	for _, p := range strings.Split(r.URL.RawQuery, "&") {
		if p != "" && !strings.HasPrefix(p, "X-Amz-Signature=") {
			k, val, _ := strings.Cut(p, "=")
			pares = append(pares, k+"="+val)
		}
	}
	sort.Strings(pares)
	var cab strings.Builder
	for _, n := range firmadas {
		val := r.Host
		if n != "host" {
			val = strings.Join(strings.Fields(strings.Join(r.Header.Values(n), ",")), " ")
		}
		cab.WriteString(n + ":" + val + "\n")
	}
	canon := r.Method + "\n" + r.URL.EscapedPath() + "\n" + strings.Join(pares, "&") + "\n" + cab.String() + "\n" + strings.Join(firmadas, ";") + "\n" + hash
	if got := f.firmaDe(fecha, canon); got != recibida {
		return 403, "SignatureDoesNotMatch"
	}
	// Cuerpo: según el hash declarado.
	cuerpo, err := io.ReadAll(r.Body)
	if err != nil {
		return 400, "bad body: " + err.Error()
	}
	switch hash {
	case hashSinFirmar, hashSinFirmarTr:
	case hashTrozos, hashTrozosTrailer:
		datos, msg := verificarTrozos(f, fecha, recibida, cuerpo, hash == hashTrozosTrailer)
		if msg != "" {
			return 403, msg
		}
		if n, _ := strconv.Atoi(r.Header.Get("X-Amz-Decoded-Content-Length")); n != len(datos) {
			return 400, "IncompleteBody"
		}
		cuerpo = datos
	default:
		s := sha256.Sum256(cuerpo)
		if hex.EncodeToString(s[:]) != hash {
			return 400, "XAmzContentSHA256Mismatch"
		}
	}
	v.mu.Lock()
	if v.cuerpos == nil {
		v.cuerpos = map[string][]byte{}
	}
	v.cuerpos[r.URL.Path] = cuerpo
	v.mu.Unlock()
	return 200, ""
}

// verificarTrozos decodifica un cuerpo aws-chunked comprobando cada firma.
func verificarTrozos(f firmador, fecha time.Time, semilla string, cuerpo []byte, trailer bool) ([]byte, string) {
	prev, resto := semilla, cuerpo
	var datos []byte
	for {
		i := bytes.Index(resto, []byte("\r\n"))
		if i < 0 {
			return nil, "IncompleteBody"
		}
		tam, sig, ok := strings.Cut(string(resto[:i]), ";chunk-signature=")
		n, err := strconv.ParseInt(tam, 16, 64)
		if !ok || err != nil {
			return nil, "InvalidChunk"
		}
		resto = resto[i+2:]
		if int64(len(resto)) < n {
			return nil, "IncompleteBody"
		}
		d := resto[:n]
		resto = resto[n:]
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + fecha.Format(formatoFecha) + "\n" + f.ambito(fecha) + "\n" + prev + "\n" + hashVacio + "\n" + hexSHA256(d)
		if want := hex.EncodeToString(hmacSHA256(f.clave(fecha), sts)); want != sig {
			return nil, "SignatureDoesNotMatch(chunk)"
		}
		prev = sig
		datos = append(datos, d...)
		if n > 0 || !trailer {
			if !bytes.HasPrefix(resto, []byte("\r\n")) {
				return nil, "InvalidChunk"
			}
			resto = resto[2:]
		}
		if n == 0 {
			break
		}
	}
	if trailer {
		var canon string
		for {
			i := bytes.Index(resto, []byte("\r\n"))
			if i < 0 {
				return nil, "IncompleteBody"
			}
			l := string(resto[:i])
			resto = resto[i+2:]
			if l == "" {
				return nil, "MissingTrailerSignature"
			}
			k, val, _ := strings.Cut(l, ":")
			if k == "x-amz-trailer-signature" {
				sts := "AWS4-HMAC-SHA256-TRAILER\n" + fecha.Format(formatoFecha) + "\n" + f.ambito(fecha) + "\n" + prev + "\n" + hexSHA256([]byte(canon))
				if want := hex.EncodeToString(hmacSHA256(f.clave(fecha), sts)); want != val {
					return nil, "SignatureDoesNotMatch(trailer)"
				}
				break
			}
			canon += k + ":" + val + "\n"
		}
		resto = bytes.TrimPrefix(resto, []byte("\r\n"))
	}
	if len(resto) != 0 {
		return nil, "TrailingBytes"
	}
	return datos, ""
}

// montajeS3 es un proxy con una credencial sigv4 para example.com cuyo
// proveedor es el verificador. aud es el registro de auditoría; logs, lo que
// el proxy escribió con Logf.
type montajeS3 struct {
	srv  *httptest.Server
	ver  *verificadorS3
	aud  string
	logs *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func nuevoMontajeS3(t *testing.T, extra ...Credential) *montajeS3 {
	t.Helper()
	m := &montajeS3{ver: &verificadorS3{}, logs: &syncBuf{}}
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.ver.manejar(t, w, r) }))
	t.Cleanup(up.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	m.aud = filepath.Join(t.TempDir(), "credaudit.jsonl")
	p := New(Options{Transport: tr, AuditPath: m.aud, Logf: func(f string, a ...any) { m.logs.Write([]byte(f)) }})
	creds := append([]Credential{{Env: "AWS_ACCESS_KEY_ID", Domain: svHost, Placeholder: svMarcAK, Secret: svRealSK,
		Kind: KindSigV4, AccessKeyID: svRealAK, Region: svRegion}}, extra...)
	if _, err := p.SetCredentials(creds); err != nil {
		t.Fatal(err)
	}
	m.srv = httptest.NewServer(p)
	t.Cleanup(func() { m.srv.Close(); p.Close() })
	return m
}

// firmaInv es la firma que pondría el cliente del invitado: con el marcador
// como access key id y SU secret key (que no es la real).
var firmaInv = firmador{accessKey: svMarcAK, secret: svMarcSK, region: svRegion, service: "s3"}

// peticionInvitado firma una petición como el cliente de la microVM y la manda
// al proxy (que escucha en HTTP, con el Host del proveedor). cabs son las
// cabeceras extra, todas firmadas.
func (m *montajeS3) peticionInvitado(t *testing.T, metodo, ruta, query, hash string, cuerpo []byte, cabs map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(metodo, m.srv.URL+ruta, nil)
	req.URL.RawQuery = query
	req.Host = svHost
	for k, v := range cabs {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Amz-Content-Sha256", hash)
	ahora := time.Now().UTC()
	// El invitado tiene la hora corrida 3 minutos: el proxy firma con la suya.
	ahora = ahora.Add(-3 * time.Minute)
	req.Header.Set("X-Amz-Date", ahora.Format(formatoFecha))
	firmadas := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for k := range cabs {
		firmadas = append(firmadas, strings.ToLower(k))
	}
	sort.Strings(firmadas)
	_, rutaCan := rutaFirmada(req.URL.Path, "s3")
	canon := peticionCanonica(metodo, rutaCan, queryCanonica(paresQuery(query)), req.Header, svHost, firmadas, hash)
	sig := firmaInv.firmaDe(ahora, canon)
	req.Header.Set("Authorization", algoSigV4+" Credential="+svMarcAK+"/"+firmaInv.ambito(ahora)+", SignedHeaders="+strings.Join(firmadas, ";")+", Signature="+sig)
	if cuerpo != nil {
		req.Body = io.NopCloser(bytes.NewReader(cuerpo))
		req.ContentLength = int64(len(cuerpo))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestSigV4ProxyCuerpoConHash(t *testing.T) {
	m := nuevoMontajeS3(t)
	cuerpo := []byte("contenido del objeto")
	h := sha256.Sum256(cuerpo)
	resp, b := m.peticionInvitado(t, "PUT", "/mi-cubo/dir uno/ñandú+x.txt", "partNumber=2&uploadId=a%2Fb", hex.EncodeToString(h[:]), cuerpo, map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s (servidor: %v)", resp.StatusCode, b, m.ver.vistas)
	}
	if string(m.ver.cuerpos["/mi-cubo/dir uno/ñandú+x.txt"]) != string(cuerpo) {
		t.Errorf("cuerpo recibido: %q", m.ver.cuerpos)
	}
	// El servidor devuelve la cabecera Authorization con el access key id real:
	// al invitado le llega el marcador.
	if eco := resp.Header.Get("X-Eco"); strings.Contains(eco, svRealAK) || !strings.Contains(eco, svMarcAK) {
		t.Errorf("el access key id real no debía llegar al invitado: %q", eco)
	}
}

func TestSigV4ProxyUnsignedPayloadYGet(t *testing.T) {
	m := nuevoMontajeS3(t)
	for _, c := range []struct{ metodo, ruta, query string }{
		{"GET", "/mi-cubo", "list-type=2&prefix=a%20b%2Bc"},
		{"GET", "/mi-cubo/a b", ""},
		{"DELETE", "/mi-cubo/x", ""},
	} {
		resp, b := m.peticionInvitado(t, c.metodo, c.ruta, c.query, hashSinFirmar, nil, nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s %s?%s: %d %s", c.metodo, c.ruta, c.query, resp.StatusCode, b)
		}
	}
	// PUT con UNSIGNED-PAYLOAD y cuerpo.
	resp, b := m.peticionInvitado(t, "PUT", "/mi-cubo/y", "", hashSinFirmar, []byte("abc"), nil)
	if resp.StatusCode != 200 || string(m.ver.cuerpos["/mi-cubo/y"]) != "abc" {
		t.Errorf("PUT unsigned: %d %s", resp.StatusCode, b)
	}
}

// Sin la re-firma, lo mismo con el marcador lo rechaza el servidor: la firma
// del invitado no vale y no se reenvía.
func TestSigV4ProxyRechazaOtroMarcador(t *testing.T) {
	m := nuevoMontajeS3(t)
	req, _ := http.NewRequest("GET", m.srv.URL+"/mi-cubo", nil)
	req.Host = svHost
	req.Header.Set("Authorization", algoSigV4+" Credential=AKIAOTROQUENOES/20260101/garage/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature="+strings.Repeat("0", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Pasa por el camino normal con la firma ajena: el servidor la rechaza.
	if resp.StatusCode != 403 {
		t.Errorf("status %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", m.srv.URL+"/mi-cubo", nil)
	req.Host = svHost
	req.Header.Set("Authorization", "AWS4-ECDSA-P256-SHA256 Credential="+svMarcAK+"/x, SignedHeaders=host, Signature=00")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("SigV4a: status %d, quería 400", resp.StatusCode)
	}
}

func TestSigV4ProxyAwsChunkedVariosTrozos(t *testing.T) {
	for _, c := range []struct {
		nombre, hash string
		trailer      bool
	}{{"firmado", hashTrozos, false}, {"con trailer", hashTrozosTrailer, true}} {
		t.Run(c.nombre, func(t *testing.T) {
			m := nuevoMontajeS3(t)
			datos := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
			trozos := [][]byte{datos[:20000], datos[20000:50000], datos[50000:]}
			// Lo que el cliente firmaría: la semilla es la firma de la petición
			// con SUS credenciales; el proxy la ignora y encadena con la suya.
			ahora := time.Now().UTC()
			semilla := strings.Repeat("e", 64)
			cuerpo := trozosInvitado(firmaInv, resultadoFirma{semilla: semilla, fecha: ahora}, trozos, c.trailer, strings.Repeat("f", 64))
			cabs := map[string]string{
				"Content-Encoding":             "aws-chunked",
				"X-Amz-Decoded-Content-Length": strconv.Itoa(len(datos)),
			}
			resp, b := m.peticionInvitado(t, "PUT", "/mi-cubo/grande", "", c.hash, cuerpo, cabs)
			if resp.StatusCode != 200 {
				t.Fatalf("%d %s (servidor: %v)", resp.StatusCode, b, m.ver.vistas)
			}
			if !bytes.Equal(m.ver.cuerpos["/mi-cubo/grande"], datos) {
				t.Errorf("el servidor recibió %d bytes distintos de los %d enviados", len(m.ver.cuerpos["/mi-cubo/grande"]), len(datos))
			}
		})
	}
}

func TestSigV4ProxyPrefirmada(t *testing.T) {
	m := nuevoMontajeS3(t)
	ahora := time.Now().UTC()
	// El invitado prefirma una URL con su marcador (sin red) y la usa con curl.
	q := url.Values{
		"X-Amz-Algorithm":     {algoSigV4},
		"X-Amz-Credential":    {svMarcAK + "/" + firmaInv.ambito(ahora)},
		"X-Amz-Date":          {ahora.Format(formatoFecha)},
		"X-Amz-Expires":       {"300"},
		"X-Amz-SignedHeaders": {"host"},
		"versionId":           {"7"},
	}
	_, rutaCan := rutaFirmada("/mi-cubo/clave nueva.txt", "s3")
	sig := firmaInv.firmaDe(ahora, peticionCanonica("GET", rutaCan, queryCanonica(paresQuery(q.Encode())), http.Header{}, svHost, []string{"host"}, hashSinFirmar))
	req, _ := http.NewRequest("GET", m.srv.URL+"/mi-cubo/clave%20nueva.txt?"+q.Encode()+"&X-Amz-Signature="+sig, nil)
	req.Host = svHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s (servidor: %v)", resp.StatusCode, b, m.ver.vistas)
	}
	// Una prefirmada caducada sigue caducada: el proxy no alarga lo que pidió el invitado.
	viejo := ahora.Add(-time.Hour)
	q.Set("X-Amz-Date", viejo.Format(formatoFecha))
	q.Set("X-Amz-Credential", svMarcAK+"/"+firmaInv.ambito(viejo))
	req, _ = http.NewRequest("GET", m.srv.URL+"/mi-cubo/clave%20nueva.txt?"+q.Encode()+"&X-Amz-Signature="+sig, nil)
	req.Host = svHost
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("prefirmada caducada: %d, quería 403", resp.StatusCode)
	}
}

// La clave real no aparece en el registro de auditoría ni en los logs del
// proxy, ni en las respuestas (incluidos los errores), tras peticiones de
// todas las formas.
func TestSigV4LaClaveRealNuncaSale(t *testing.T) {
	m := nuevoMontajeS3(t)
	h := sha256.Sum256([]byte("x"))
	m.peticionInvitado(t, "PUT", "/mi-cubo/a", "", hex.EncodeToString(h[:]), []byte("x"), nil)
	m.peticionInvitado(t, "GET", "/mi-cubo/a", "X=1", hashSinFirmar, nil, nil)
	// Una firma mal hecha provoca un error del servidor: tampoco lleva la clave.
	resp, b := m.peticionInvitado(t, "PUT", "/mi-cubo/a", "", strings.Repeat("0", 64), []byte("x"), nil)
	if resp.StatusCode == 200 {
		t.Fatal("un hash que no es el del cuerpo debía fallar")
	}
	for _, s := range []string{svRealSK, hexSHA256([]byte(svRealSK))} {
		if strings.Contains(string(b), s) {
			t.Errorf("la respuesta lleva la clave: %s", b)
		}
	}
	m.srv.Close() // el registro se escribe al terminar cada petición; se lee entero
	aud, err := os.ReadFile(m.aud)
	if err != nil {
		t.Fatal(err)
	}
	if len(aud) == 0 {
		t.Fatal("registro de auditoría vacío")
	}
	for _, donde := range []struct{ n, v string }{{"auditoría", string(aud)}, {"logs", m.logs.String()}} {
		for _, s := range []string{svRealSK, svRealAK, hex.EncodeToString(m.firmaDeLaClave()), url.QueryEscape(svRealSK)} {
			if strings.Contains(donde.v, s) {
				t.Errorf("%s lleva %q", donde.n, s)
			}
		}
	}
	for _, linea := range m.ver.vistas {
		if strings.Contains(linea, svRealSK) {
			t.Errorf("el servidor vio la clave en la URL: %s", linea)
		}
	}
	if !strings.Contains(string(aud), `"creds":["AWS_ACCESS_KEY_ID"]`) {
		t.Errorf("la auditoría debía anotar la credencial usada: %s", aud)
	}
}

// firmaDeLaClave es la clave de firma del día: la otra cosa derivada de la
// clave real que tampoco debe verse.
func (m *montajeS3) firmaDeLaClave() []byte {
	return firmador{secret: svRealSK, region: svRegion, service: "s3"}.clave(time.Now().UTC())
}

// Sin hash declarado (otros servicios): lo calcula el proxy.
func TestSigV4SinHashDeclaradoSeCalcula(t *testing.T) {
	cuerpo := []byte("Param1=v")
	f := firmador{accessKey: svRealAK, secret: svRealSK, region: svRegion, service: "execute-api"}
	var visto string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		fecha, _ := time.Parse(formatoFecha, r.Header.Get("X-Amz-Date"))
		_, rutaCan := rutaFirmada(r.URL.Path, "execute-api")
		firmadas := strings.Split(strings.Split(strings.Split(auth, "SignedHeaders=")[1], ",")[0], ";")
		canon := peticionCanonica(r.Method, rutaCan, queryCanonica(paresQuery(r.URL.RawQuery)), r.Header, r.Host, firmadas, hexSHA256(b))
		if !strings.HasSuffix(auth, "Signature="+f.firmaDe(fecha, canon)) {
			http.Error(w, "SignatureDoesNotMatch", 403)
			return
		}
		visto = string(b)
	}))
	t.Cleanup(up.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	p := New(Options{Transport: tr})
	if _, err := p.SetCredentials([]Credential{{Env: "K", Domain: svHost, Placeholder: svMarcAK, Secret: svRealSK,
		Kind: KindSigV4, AccessKeyID: svRealAK, Region: svRegion, Service: "execute-api"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("POST", srv.URL+"/a b/../c?z=1&a=2", bytes.NewReader(cuerpo))
	req.Host = svHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Date", time.Now().UTC().Format(formatoFecha))
	req.Header.Set("Authorization", algoSigV4+" Credential="+svMarcAK+"/20260101/garage/execute-api/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature="+strings.Repeat("0", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || visto != string(cuerpo) {
		t.Fatalf("status %d, cuerpo visto %q", resp.StatusCode, visto)
	}
}

func TestSigV4ValidacionDeLaCredencial(t *testing.T) {
	ok := Credential{Domain: "example.com", Placeholder: svMarcAK, Secret: svRealSK, Kind: KindSigV4, AccessKeyID: svRealAK, Region: "auto"}
	if err := ValidarCredenciales([]Credential{ok}); err != nil {
		t.Fatal(err)
	}
	c := ok
	if err := ValidarCredenciales([]Credential{c}); err != nil || c.Service != "" {
		// Se normaliza sobre el slice, no sobre la copia: aquí solo se ve que vale.
		t.Log("servicio por defecto en el slice")
	}
	malas := map[string]func(*Credential){
		"sin access key id": func(c *Credential) { c.AccessKeyID = "" },
		"sin región":        func(c *Credential) { c.Region = "" },
		"con -body":         func(c *Credential) { c.Body = true },
		"con upstream":      func(c *Credential) { c.Upstream = "127.0.0.1:3900" },
		"servicio raro":     func(c *Credential) { c.Service = "s3/../x" },
	}
	for n, mod := range malas {
		c := ok
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: debía rechazarse", n)
		}
	}
	// Los campos de sigv4 en una credencial HTTP no valen.
	h := Credential{Domain: "example.com", Placeholder: testPlace, Secret: testSecret, Region: "x"}
	if err := ValidarCredenciales([]Credential{h}); err == nil {
		t.Error("región en una credencial http debía rechazarse")
	}
}

var _ = svMaxTest
