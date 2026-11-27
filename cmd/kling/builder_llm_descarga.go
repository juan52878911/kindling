package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DESCARGA DE LO QUE VA A LA IMAGEN DE UN MODELO (constructor "llm").
//
// Un CDN (HuggingFace, GitHub) pone un techo por conexión: 41-57 MiB/s medidos
// desde EC2, muy por debajo del disco y de la red. Si el servidor sabe de
// rangos, el fichero se pide en trozos con varias peticiones a la vez y cada
// una escribe su parte en su sitio del .part; el sha256 se calcula al final,
// sobre el fichero entero (que aún está en la caché de páginas). Si no sabe
// (contesta 200 a la primera petición con rango), se baja de un flujo con esa
// misma respuesta, hasheando al pasar.
//
// Sin fsync del .part: un fichero que un corte deja roto no pasa el hash y se
// vuelve a bajar. Por eso mismo el sello (abajo) solo vale dentro del arranque
// en que se escribió.

// flujos es cuántas peticiones con rango van a la vez y tamTrozo cuánto pide
// cada una; variables para los tests.
var (
	flujos   = 8
	tamTrozo = int64(16 << 20)
)

// maxDescarga es el tope de lo que un servidor puede decir que mide un
// fichero: el .part es disperso, pero un Content-Range absurdo no debería
// llegar a reservar nada.
const maxDescarga = int64(256 << 30)

// intentosTrozo es cuántas veces se pide un trozo antes de dar la descarga por
// fallida (un CDN corta a veces una conexión suelta).
const intentosTrozo = 3

// hashFichero calcula el sha256 de un fichero; variable para que los tests
// cuenten cuántas veces se relee entero.
var hashFichero = sha256File

// bootID es el identificador del arranque del núcleo ("" si no lo hay: fuera de
// Linux, y entonces no hay sello); variable para los tests.
var bootID = func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// stallTimeout es cuánto puede estar una descarga sin recibir ni un byte
// antes de darla por muerta. No es un timeout total: un GGUF de varios GB en
// una red lenta puede tardar minutos de forma legítima, y cortarlo por tiempo
// total rompería esa descarga a mitad. Lo que sí es señal de que el otro lado
// (HuggingFace, GitHub) se quedó colgado es que pasen 60s sin ni un byte.
const stallTimeout = 60 * time.Second

// fetchVerified deja en dst el fichero de url con ese sha256. Si dst ya está y
// cuadra, no descarga nada. Escribe al lado y renombra: un corte a medias no
// deja en la caché un fichero que parezca bueno.
func fetchVerified(url, want, dst string) error {
	return fetchVerifiedTimeout(url, want, dst, stallTimeout)
}

// fetchVerifiedTimeout es fetchVerified con el detector de estancamiento
// configurable, para poder probarlo sin esperar 60s de verdad.
func fetchVerifiedTimeout(url, want, dst string, stall time.Duration) error {
	if ok, _ := verificarCache(dst, want); ok {
		return nil
	}
	os.Remove(dst)
	os.Remove(dst + selloExt)
	tmp := dst + ".part"
	defer os.Remove(tmp)
	t0 := time.Now()
	got, nb, n, err := descargar(url, tmp, stall)
	if err != nil {
		return err
	}
	if got == "" {
		// Por trozos: el hash, ahora, del fichero entero.
		if got, err = hashFichero(tmp); err != nil {
			return err
		}
	}
	if got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", url, got, want)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	escribirSello(dst, want)
	fmt.Printf("downloaded %s: %d MiB in %.1f s (%d stream%s), sha256 verified\n",
		path.Base(dst), nb>>20, time.Since(t0).Seconds(), n, map[bool]string{true: "s"}[n != 1])
	return nil
}

// verificarCache dice si p es el fichero con ese sha256: por el sello si lo
// hay y vale, o releyéndolo entero (y entonces lo sella).
func verificarCache(p, want string) (bool, error) {
	if selloValido(p, want) {
		return true, nil
	}
	got, err := hashFichero(p)
	if err != nil {
		return false, err
	}
	if got != want {
		return false, nil
	}
	escribirSello(p, want)
	return true, nil
}

// EL SELLO: <fichero>.verified apunta el sha256 que se comprobó y cómo era el
// fichero entonces (tamaño, mtime, inodo) y en qué arranque. Mientras todo eso
// siga igual, el contenido es el que se hasheó: la caché es de root (0700) y
// lo que cambie el fichero cambia su mtime. El arranque está porque, sin
// fsync, tras un corte el disco puede tener otra cosa que lo que se verificó
// en memoria: en un arranque nuevo se rehashea una vez. Nunca sustituye la
// verificación de una descarga: solo se escribe después de ella.
const selloExt = ".verified"

type sello struct {
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	Ino     uint64 `json:"ino"`
	Boot    string `json:"boot"`
}

func selloDe(p, sha string) (sello, bool) {
	boot := bootID()
	fi, err := os.Stat(p)
	if err != nil || boot == "" || !fi.Mode().IsRegular() {
		return sello{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return sello{}, false
	}
	return sello{SHA256: sha, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), Ino: uint64(st.Ino), Boot: boot}, true
}

func selloValido(p, want string) bool {
	b, err := os.ReadFile(p + selloExt)
	if err != nil || len(b) > 4096 {
		return false
	}
	var s sello
	if json.Unmarshal(b, &s) != nil {
		return false
	}
	ahora, ok := selloDe(p, want)
	return ok && s == ahora
}

// escribirSello es lo mejor posible: sin sello, la próxima vez se rehashea.
func escribirSello(p, sha string) {
	s, ok := selloDe(p, sha)
	if !ok {
		return
	}
	b, _ := json.Marshal(s)
	tmp := p + selloExt + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil || os.Rename(tmp, p+selloExt) != nil {
		os.Remove(tmp)
	}
}

func clienteDescarga() *http.Client {
	// Timeouts de conexión, TLS y cabeceras: si el servidor ni siquiera llega
	// a responder, no nos quedamos esperando antes de empezar a recibir.
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   flujos,
		},
	}
}

// descargar deja en tmp el contenido de url. Devuelve el sha256 si lo pudo
// calcular al pasar (un solo flujo; "" por trozos), los bytes y cuántos flujos
// usó.
func descargar(url, tmp string, stall time.Duration) (string, int64, int, error) {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	client := clienteDescarga()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := pedir(ctx, client, url, 0, tamTrozo-1)
	if err != nil {
		return "", 0, 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// Sin rangos: esta misma respuesta, entera.
		defer resp.Body.Close()
		h := sha256.New()
		nb, err := copiarVigilado(ctx, cancel, io.MultiWriter(f, h), resp.Body, stall, url)
		if err != nil {
			return "", 0, 1, err
		}
		return hex.EncodeToString(h.Sum(nil)), nb, 1, f.Close()
	case http.StatusPartialContent:
	default:
		resp.Body.Close()
		return "", 0, 0, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	ini, fin, total, err := contentRange(resp.Header.Get("Content-Range"))
	if err != nil || ini != 0 || (fin != tamTrozo-1 && fin != total-1) {
		resp.Body.Close()
		return "", 0, 0, fmt.Errorf("GET %s: bad Content-Range %q", url, resp.Header.Get("Content-Range"))
	}
	if total > maxDescarga {
		resp.Body.Close()
		return "", 0, 0, fmt.Errorf("GET %s: the server says the file is %d bytes", url, total)
	}
	// Los demás trozos, a la URL final (tras las redirecciones del CDN), y a la
	// original si hay que repetir uno: la firmada puede caducar.
	final := resp.Request.URL.String()
	if err := f.Truncate(total); err != nil {
		resp.Body.Close()
		return "", 0, 0, err
	}
	_, err = copiarVigilado(ctx, cancel, exacto(io.NewOffsetWriter(f, 0), fin+1), resp.Body, stall, url)
	resp.Body.Close()
	if err != nil {
		return "", 0, 0, err
	}

	trozos := make(chan int64)
	var (
		mu      sync.Mutex
		primero error
		wg      sync.WaitGroup
	)
	fallar := func(err error) {
		mu.Lock()
		if primero == nil {
			primero = err
			cancel()
		}
		mu.Unlock()
	}
	n := int((total - fin - 1 + tamTrozo - 1) / tamTrozo)
	if n > flujos {
		n = flujos
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for off := range trozos {
				end := off + tamTrozo - 1
				if end >= total {
					end = total - 1
				}
				if err := trozo(ctx, client, []string{final, url}, f, off, end, total, stall); err != nil {
					fallar(err)
				}
			}
		}()
	}
envio:
	for off := fin + 1; off < total; off += tamTrozo {
		select {
		case trozos <- off:
		case <-ctx.Done():
			break envio
		}
	}
	close(trozos)
	wg.Wait()
	if primero != nil {
		return "", 0, 0, primero
	}
	return "", total, n + 1, f.Close()
}

// trozo baja [ini, fin] de url a su sitio en f, con unos pocos reintentos.
func trozo(ctx context.Context, client *http.Client, urls []string, f *os.File, ini, fin, total int64, stall time.Duration) error {
	var last error
	for i := 0; i < intentosTrozo; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u := urls[0]
		if i > 0 {
			u = urls[len(urls)-1]
		}
		last = trozoUnaVez(ctx, client, u, f, ini, fin, total, stall)
		if last == nil {
			return nil
		}
	}
	return last
}

func trozoUnaVez(ctx context.Context, client *http.Client, url string, f *os.File, ini, fin, total int64, stall time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := pedir(ctx, client, url, ini, fin)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("GET %s (bytes %d-%d): %s", url, ini, fin, resp.Status)
	}
	a, b, t, err := contentRange(resp.Header.Get("Content-Range"))
	if err != nil || a != ini || b != fin || t != total {
		return fmt.Errorf("GET %s: bad Content-Range %q for bytes %d-%d/%d", url, resp.Header.Get("Content-Range"), ini, fin, total)
	}
	_, err = copiarVigilado(ctx, cancel, exacto(io.NewOffsetWriter(f, ini), fin-ini+1), resp.Body, stall, url)
	return err
}

func pedir(ctx context.Context, client *http.Client, url string, ini, fin int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", ini, fin))
	// Sin compresión de transporte: los rangos son del fichero, no de lo
	// comprimido.
	req.Header.Set("Accept-Encoding", "identity")
	return client.Do(req)
}

// contentRange lee "bytes INI-FIN/TOTAL".
func contentRange(v string) (ini, fin, total int64, err error) {
	if _, err = fmt.Sscanf(v, "bytes %d-%d/%d", &ini, &fin, &total); err != nil {
		return 0, 0, 0, err
	}
	if ini < 0 || fin < ini || total <= fin {
		return 0, 0, 0, fmt.Errorf("bad range")
	}
	return ini, fin, total, nil
}

// exacto es un escritor que acepta exactamente n bytes: ni uno más (un
// servidor que manda de más no pisa el trozo siguiente) ni uno menos (Close).
type escritorExacto struct {
	w    io.Writer
	left int64
}

func exacto(w io.Writer, n int64) *escritorExacto { return &escritorExacto{w, n} }

func (e *escritorExacto) Write(p []byte) (int, error) {
	if int64(len(p)) > e.left {
		return 0, errors.New("the server sent more bytes than the range")
	}
	n, err := e.w.Write(p)
	e.left -= int64(n)
	return n, err
}

// copiarVigilado copia de body a w cortando si pasan stall sin bytes.
func copiarVigilado(ctx context.Context, cancel func(), w io.Writer, body io.Reader, stall time.Duration, url string) (int64, error) {
	// Detector de estancamiento: cada byte que llega reinicia el temporizador;
	// si se agota, cancela el contexto de la petición y la copia termina con
	// error en vez de colgar el constructor para siempre.
	timer := time.AfterFunc(stall, cancel)
	defer timer.Stop()
	nb, err := io.Copy(w, &stallReader{r: body, timer: timer, d: stall})
	if err == nil {
		if e, ok := w.(*escritorExacto); ok && e.left != 0 {
			err = fmt.Errorf("short range: %d bytes missing", e.left)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nb, fmt.Errorf("downloading %s: stalled, no data for %s", url, stall)
		}
		return nb, fmt.Errorf("downloading %s: %w", url, err)
	}
	return nb, nil
}

// stallReader reinicia timer con cada lectura que trae bytes; si no llega
// ninguna a tiempo, el AfterFunc de timer dispara y corta la descarga.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}
