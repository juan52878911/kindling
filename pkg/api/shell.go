package api

// Shell interactiva dentro de una microVM.
//
// exec (exec.go) sirve para lanzar un comando y ver su salida: la entrada viaja
// entera en la petición y la salida vuelve en un solo sentido. Una shell necesita
// lo contrario —bytes en los dos sentidos, para siempre— así que usa otro
// camino: la petición pide cambiar de protocolo (Upgrade), el servidor contesta
// 101 y a partir de ahí la conexión transporta TRAMAS binarias en ambos
// sentidos, como hacen `docker exec -it` o `kubectl exec -it`.
//
// Se eligió esto y no un cuerpo HTTP en streaming por dos razones que están en el
// código: los plazos de lectura del daemon (30 s) y del agente (120 s) cortarían
// la sesión, y desaparecen al secuestrar la conexión; y el túnel SSH cierra el
// socket entero en cuanto una de sus dos copias termina, así que un cierre de
// stdin se llevaría por delante la salida.
//
// Mismo protocolo en los dos saltos: CLI→daemon y daemon→invitado.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ShellProto es el nombre del protocolo en la cabecera Upgrade. La versión va
// dentro: un cliente nuevo contra un daemon viejo recibe 426 y un mensaje claro,
// en vez de hablar dos idiomas por el mismo socket.
const ShellProto = "kling-shell/1"

// Tipos de trama. El sentido importa: el daemon rechaza lo que no corresponda,
// porque el invitado se considera hostil.
const (
	ShellData   byte = 0 // bytes de stdin (hacia dentro) o de la salida del PTY (hacia fuera)
	ShellResize byte = 1 // rows uint16, cols uint16 (solo hacia dentro)
	ShellSignal byte = 2 // un número de señal (solo hacia dentro)
	ShellExit   byte = 3 // código de salida int32 (solo hacia fuera). Siempre la última
	ShellError  byte = 4 // texto UTF-8 (solo hacia fuera). También la última
	ShellPing   byte = 5 // vacía, del daemon al cliente: detecta conexiones muertas
)

// ShellMaxFrame es la carga máxima de una trama. Suficiente para una pantalla
// entera de salida y pequeña para que nadie pueda hacer que el otro extremo
// reserve memoria a su antojo.
const ShellMaxFrame = 64 << 10

// ShellRequest es lo que se manda en el cuerpo de la petición de upgrade.
type ShellRequest struct {
	// Cmd es argv. Vacío = la shell de inicio de sesión del invitado.
	Cmd []string `json:"cmd,omitempty"`
	Dir string   `json:"dir,omitempty"`
	Env []string `json:"env,omitempty"`
	// Term es el TERM que verá el programa de dentro.
	Term string `json:"term,omitempty"`
	// Rows y Cols son el tamaño inicial de la terminal. 0 = 24x80.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// ShellDefaultCmd es lo que se ejecuta si no se pide otra cosa. `sh` y no
// `bash`: la base de las imágenes es Alpine, donde bash puede no estar.
var ShellDefaultCmd = []string{"/bin/sh", "-l"}

// WriteFrame escribe una trama. No es concurrente: quien escriba desde varias
// goroutines tiene que serializar.
func WriteFrame(w io.Writer, t byte, payload []byte) error {
	if len(payload) > ShellMaxFrame {
		return fmt.Errorf("frame of %d bytes; the limit is %d", len(payload), ShellMaxFrame)
	}
	var head [5]byte
	head[0] = t
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame lee una trama. El tope no es una cortesía: sin él, el otro extremo
// decide cuánta memoria reservamos.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > ShellMaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes; the limit is %d", n, ShellMaxFrame)
	}
	if n == 0 {
		return head[0], nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return head[0], buf, nil
}

// ResizePayload y ExitPayload son las cargas de sus tramas.
func ResizePayload(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return b[:]
}

// ParseResize lee una carga de redimensionado.
func ParseResize(p []byte) (rows, cols uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("resize frame of %d bytes; it has to be 4", len(p))
	}
	return binary.BigEndian.Uint16(p[0:]), binary.BigEndian.Uint16(p[2:]), nil
}

func ExitPayload(code int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code))
	return b[:]
}

// ParseExit lee un código de salida.
func ParseExit(p []byte) (int32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("exit frame of %d bytes; it has to be 4", len(p))
	}
	return int32(binary.BigEndian.Uint32(p)), nil
}

// ErrShellUnsupported es lo que devuelve Shell cuando el otro extremo no habla
// este protocolo: un daemon anterior a v0.7, o una imagen cuyo agente no tiene
// la ruta.
var ErrShellUnsupported = errors.New("the other end does not speak " + ShellProto)

// ShellConn es una sesión de shell abierta: tramas en los dos sentidos hasta que
// alguien cuelga.
type ShellConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
}

// Read y Write de tramas.
func (c *ShellConn) ReadFrame() (byte, []byte, error) { return ReadFrame(c.br) }
func (c *ShellConn) WriteFrame(t byte, p []byte) error {
	return WriteFrame(c.rwc, t, p)
}
func (c *ShellConn) Close() error { return c.rwc.Close() }

// Shell abre una shell dentro de la máquina.
//
// Al volver, la conexión ya no la gestiona el http.Client: cancelar el contexto
// NO la cierra, así que quien llama tiene que cerrarla él en todos los caminos.
func (c *Client) Shell(ctx context.Context, ref string, r ShellRequest) (*ShellConn, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://kling/machines/"+url.PathEscape(ref)+"/shell", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", ShellProto)

	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUpgradeRequired || resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrShellUnsupported, resp.Status)
		}
		return nil, statusErrorFrom(resp)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("the connection could not be taken over for %s", ShellProto)
	}
	return &ShellConn{rwc: rwc, br: bufio.NewReaderSize(rwc, 64<<10)}, nil
}
