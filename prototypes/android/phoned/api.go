package main

// La API del teléfono (docs/phoned.md). HTTP/1.1 en un puerto del invitado:
// hoy se llega por el proxy del daemon (POST /machines/{ref}/guest), mañana
// por una arista de grafo o un reenvío; por eso es HTTP plano, sin nada del
// daemon, y todo cuerpo binario admite base64 (el proxy lleva el cuerpo como
// una cadena JSON, que no es binario limpio): ?encoding=base64.
//
//	GET  /v1/health                      200 | 503 (JSON)
//	GET  /v1/screen                      PNG
//	GET  /v1/tree[?compressed=1]         XML (uidump si está; si no, uiautomator dump)
//	POST /v1/tap      {"x":N,"y":N}
//	POST /v1/swipe    {"x1":N,"y1":N,"x2":N,"y2":N,"ms":N}
//	POST /v1/text     {"text":"..."}
//	POST /v1/key      {"key":"BACK" | "4"}
//	POST /v1/install  cuerpo = el APK
//	POST /v1/launch   {"package":"com.termux"}  (su actividad de LAUNCHER)
//	GET  /v1/logs[?buffer=main|system|crash|events|all|phoned][&lines=N]
//	GET  /v1/identity                    serie, android_id, adb, SSAID (JSON)
//
// No hay shell: todo lo de arriba se ejecuta con argumentos validados aquí.
// Quien necesite una orden arbitraria usa `kling exec` + android-sh, que
// exige allow_exec (lo que es: root en el teléfono).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// phoneOps es lo que la API necesita del teléfono: en Linux lo da el
// supervisor (android_linux.go); en las pruebas, un falso.
type phoneOps interface {
	Health(ctx context.Context) healthInfo
	Screen(ctx context.Context) ([]byte, error)
	Tree(ctx context.Context, compressed bool) (xml []byte, source string, err error)
	Input(ctx context.Context, in inputReq) (via string, err error)
	Install(ctx context.Context, apk []byte) (string, error)
	Launch(ctx context.Context, pkg string) (string, error)
	Logs(ctx context.Context, buffer string, lines int) ([]byte, error)
	Identity(ctx context.Context) (identityInfo, error)
}

type healthInfo struct {
	OK            bool   `json:"ok"`
	State         string `json:"state"`
	BootCompleted bool   `json:"boot_completed"`
	SystemServer  bool   `json:"system_server"`
	AndroidPID    int    `json:"android_pid,omitempty"`
	Restarts      int    `json:"restarts"`
	UptimeS       int64  `json:"android_uptime_s"`
	Net           string `json:"net"`
	Verity        string `json:"verity"`
	Uidump        bool   `json:"uidump"`
	AdbSecure     bool   `json:"adb_secure"`
	Version       string `json:"version"`
	Detail        string `json:"detail,omitempty"`
}

type identityInfo struct {
	Serial        string            `json:"serial"`
	AndroidID     string            `json:"android_id"`
	DeviceName    string            `json:"device_name"`
	AdbSecure     bool              `json:"adb_secure"`
	AdbKeys       int               `json:"adb_keys"`
	SSAIDUserKey  string            `json:"ssaid_userkey_sha256,omitempty"`
	SSAIDPackages map[string]string `json:"ssaid,omitempty"`
}

// inputReq es un gesto ya validado.
type inputReq struct {
	Kind         string // tap | swipe | text | key
	X, Y, X2, Y2 int
	MS           int
	Text         string
	Key          string
}

const (
	maxAPKBytes  = 128 << 20
	maxTextBytes = 4096
	maxLogLines  = 5000
	defLogLines  = 200
)

var (
	reKeyName = regexp.MustCompile(`^(KEYCODE_)?[A-Z0-9_]{1,40}$`)
	reKeyNum  = regexp.MustCompile(`^[0-9]{1,3}$`)
	rePackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)
)

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &httpErr{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

// newAPI monta las rutas sobre ops.
func newAPI(ops phoneOps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		h := ops.Health(r.Context())
		code := http.StatusOK
		if !h.OK {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, h)
	})
	mux.HandleFunc("GET /v1/screen", func(w http.ResponseWriter, r *http.Request) {
		png, err := ops.Screen(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeBytes(w, r, "image/png", png)
	})
	mux.HandleFunc("GET /v1/tree", func(w http.ResponseWriter, r *http.Request) {
		xml, src, err := ops.Tree(r.Context(), r.URL.Query().Get("compressed") == "1")
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("X-Phoned-Source", src)
		writeBytes(w, r, "application/xml; charset=utf-8", xml)
	})
	for _, k := range []string{"tap", "swipe", "text", "key"} {
		kind := k
		mux.HandleFunc("POST /v1/"+kind, func(w http.ResponseWriter, r *http.Request) {
			in, err := parseInput(kind, r.Body)
			if err != nil {
				writeErr(w, err)
				return
			}
			via, err := ops.Input(r.Context(), in)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "via": via})
		})
	}
	mux.HandleFunc("POST /v1/install", func(w http.ResponseWriter, r *http.Request) {
		apk, err := readBody(r, maxAPKBytes)
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(apk) < 4 || string(apk[:4]) != "PK\x03\x04" {
			writeErr(w, badRequest("body is not an APK (zip magic missing)"))
			return
		}
		out, err := ops.Install(r.Context(), apk)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error(), "output": out})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": out, "bytes": len(apk)})
	})
	mux.HandleFunc("POST /v1/launch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Package string `json:"package"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil ||
			len(req.Package) > 200 || !rePackage.MatchString(req.Package) {
			writeErr(w, badRequest("body must be {\"package\": \"com.example.app\"}"))
			return
		}
		out, err := ops.Launch(r.Context(), req.Package)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": out})
	})
	mux.HandleFunc("GET /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		buf := q.Get("buffer")
		if buf == "" {
			buf = "main"
		}
		switch buf {
		case "main", "system", "crash", "events", "all", "phoned":
		default:
			writeErr(w, badRequest("buffer must be main, system, crash, events, all or phoned"))
			return
		}
		lines := defLogLines
		if s := q.Get("lines"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > maxLogLines {
				writeErr(w, badRequest("lines must be 1-%d", maxLogLines))
				return
			}
			lines = n
		}
		out, err := ops.Logs(r.Context(), buf, lines)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("GET /v1/identity", func(w http.ResponseWriter, r *http.Request) {
		id, err := ops.Identity(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, id)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "kling-phoned", "version": version,
			"routes": []string{"GET /v1/health", "GET /v1/screen", "GET /v1/tree", "POST /v1/tap",
				"POST /v1/swipe", "POST /v1/text", "POST /v1/key", "POST /v1/install", "POST /v1/launch", "GET /v1/logs", "GET /v1/identity"},
		})
	})
	return mux
}

func parseInput(kind string, body io.Reader) (inputReq, error) {
	var raw struct {
		X, Y, X1, Y1, X2, Y2 *int
		MS                   int
		Text                 *string
		Key                  json.RawMessage
	}
	dec := json.NewDecoder(io.LimitReader(body, 16<<10))
	if err := dec.Decode(&raw); err != nil {
		return inputReq{}, badRequest("body must be a JSON object: %v", err)
	}
	coord := func(p *int, name string) (int, error) {
		if p == nil {
			return 0, badRequest("%s is required", name)
		}
		if *p < 0 || *p > 16384 {
			return 0, badRequest("%s out of range", name)
		}
		return *p, nil
	}
	in := inputReq{Kind: kind}
	var err error
	switch kind {
	case "tap":
		if in.X, err = coord(raw.X, "x"); err != nil {
			return in, err
		}
		if in.Y, err = coord(raw.Y, "y"); err != nil {
			return in, err
		}
	case "swipe":
		x1, y1 := raw.X1, raw.Y1
		if x1 == nil {
			x1 = raw.X
		}
		if y1 == nil {
			y1 = raw.Y
		}
		if in.X, err = coord(x1, "x1"); err != nil {
			return in, err
		}
		if in.Y, err = coord(y1, "y1"); err != nil {
			return in, err
		}
		if in.X2, err = coord(raw.X2, "x2"); err != nil {
			return in, err
		}
		if in.Y2, err = coord(raw.Y2, "y2"); err != nil {
			return in, err
		}
		in.MS = raw.MS
		if in.MS == 0 {
			in.MS = 300
		}
		if in.MS < 1 || in.MS > 10000 {
			return in, badRequest("ms must be 1-10000")
		}
	case "text":
		if raw.Text == nil || *raw.Text == "" {
			return in, badRequest("text is required")
		}
		t := *raw.Text
		if len(t) > maxTextBytes {
			return in, badRequest("text longer than %d bytes", maxTextBytes)
		}
		if strings.ContainsAny(t, "\x00\r\n") {
			return in, badRequest("text must be a single line (use key ENTER)")
		}
		in.Text = t
	case "key":
		var k string
		if len(raw.Key) > 0 && raw.Key[0] == '"' {
			_ = json.Unmarshal(raw.Key, &k)
		} else {
			k = string(raw.Key)
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if !reKeyName.MatchString(k) && !reKeyNum.MatchString(k) {
			return in, badRequest("key must be a key code (4) or name (BACK, KEYCODE_HOME)")
		}
		in.Key = k
	}
	return in, nil
}

// readBody lee el cuerpo entero, con tope; con ?encoding=base64 lo decodifica.
func readBody(r *http.Request, max int64) ([]byte, error) {
	var src io.Reader = r.Body
	b64 := r.URL.Query().Get("encoding") == "base64"
	if b64 {
		src = base64.NewDecoder(base64.StdEncoding, newlineStripper{r.Body})
	}
	b, err := io.ReadAll(io.LimitReader(src, max+1))
	if err != nil {
		if b64 {
			return nil, badRequest("body is not valid base64")
		}
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, &httpErr{http.StatusRequestEntityTooLarge, fmt.Sprintf("body larger than %d bytes", max)}
	}
	return b, nil
}

// newlineStripper quita saltos de línea (base64 de `base64 -w76`).
type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	for {
		k, err := n.r.Read(p)
		j := 0
		for _, c := range p[:k] {
			if c != '\n' && c != '\r' {
				p[j] = c
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

func writeBytes(w http.ResponseWriter, r *http.Request, ctype string, b []byte) {
	if r.URL.Query().Get("encoding") == "base64" {
		w.Header().Set("Content-Type", "text/plain; charset=us-ascii")
		w.Header().Set("X-Phoned-Content-Type", ctype)
		_, _ = io.WriteString(w, base64.StdEncoding.EncodeToString(b))
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var he *httpErr
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.Is(err, errNotRunning):
		code = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// errNotRunning: Android no está arrancado (o se está relanzando).
var errNotRunning = errors.New("android is not running")

// apiServer es el servidor HTTP de la API, con plazos.
func apiServer(ops phoneOps) *http.Server {
	return &http.Server{
		Handler:           newAPI(ops),
		ReadHeaderTimeout: 10 * time.Second,
		// Un APK de 100 MiB en base64 por el proxy tarda; nada de ReadTimeout corto.
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
}
