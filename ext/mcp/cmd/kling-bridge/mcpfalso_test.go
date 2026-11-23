package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// Servidor MCP de stdio falso, hecho con el propio binario de los tests: con
// KLING_BRIDGE_MCP_FALSO=1 el binario no corre los tests, hace de servidor.
//
// Hace falta algo más que `cat`: el eco devuelve la petición tal cual, con su
// method, y un mensaje con method es justo lo que el puente debe tratar como
// iniciado por el servidor. Este contesta cada petición con un result y su
// mismo id, y con la herramienta "muestrea" hace lo que hace un servidor real
// que usa sampling: a mitad del tools/call le PIDE algo al cliente
// —con el mismo id que la llamada en vuelo, que es el caso que confundía al
// puente— y solo contesta la llamada cuando le llega la respuesta.
const envMCPFalso = "KLING_BRIDGE_MCP_FALSO"

// envHTTPFalso convierte el binario en un servidor HTTP mínimo que escucha en
// esa dirección y contesta "<pid> <SECRETO_FALSO>": hace de servidor MCP HTTP
// del modo proxy y de Chromium compartido, para ver si se relanzan y con qué
// entorno.
const envHTTPFalso = "KLING_BRIDGE_HTTP_FALSO"

// envArranqueFalso: milisegundos que el servidor MCP falso tarda en arrancar.
const envArranqueFalso = "KLING_BRIDGE_MCP_FALSO_ARRANQUE_MS"

func TestMain(m *testing.M) {
	if addr := os.Getenv(envHTTPFalso); addr != "" {
		_ = http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "%d %s", os.Getpid(), os.Getenv("SECRETO_FALSO"))
		}))
		os.Exit(1)
	}
	if os.Getenv(envMCPFalso) == "1" {
		servidorMCPFalso()
		os.Exit(0)
	}
	// Los hijos heredan el entorno (b.env vacío en los tests): con esto, lanzar
	// el propio binario ES lanzar el servidor falso. fakeMCPArgv (warm_test.go)
	// pasa a ser él: contesta el ping del calentamiento como cualquier petición.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv(envMCPFalso, "1")
	fakeMCPArgv = []string{exe}
	os.Exit(m.Run())
}

type mensajeFalso struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
	Result json.RawMessage `json:"result,omitempty"`
}

func servidorMCPFalso() {
	// Con envArranqueFalso, tarda esos milisegundos en «arrancar el runtime»
	// antes de leer nada, como node importando sus módulos.
	if ms, err := strconv.Atoi(os.Getenv(envArranqueFalso)); err == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	escribir := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	for sc.Scan() {
		var m mensajeFalso
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			// Una línea que no es JSON (un mensaje partido): un servidor real la
			// ignora o se queja por stderr; no contesta nada.
			fmt.Fprintf(os.Stderr, "mcp falso: línea que no es JSON: %q\n", sc.Text())
			continue
		}
		if m.Method == "" || len(m.ID) == 0 {
			continue // notificación, o respuesta que no esperábamos
		}
		if m.Method == "tools/call" && m.Params.Name == "muestrea" {
			escribir(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"method": "sampling/createMessage",
				"params": map[string]any{"maxTokens": 8, "messages": []any{}}})
			// Espera la respuesta del cliente: mismo id, sin method.
			var texto string
			for sc.Scan() {
				var r struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Result struct {
						Content struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				if json.Unmarshal(sc.Bytes(), &r) == nil && r.Method == "" &&
					string(r.ID) == string(m.ID) {
					texto = r.Result.Content.Text
					break
				}
			}
			escribir(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"result": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "muestreado: " + texto}}}})
			continue
		}
		escribir(map[string]any{"jsonrpc": "2.0", "id": m.ID,
			"result": map[string]any{"eco": m.Method}})
	}
}
