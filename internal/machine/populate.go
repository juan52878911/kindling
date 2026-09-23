package machine

// Poblar un volumen ejecutando el instalador DENTRO de una microVM.
//
// Instalar paquetes es ejecutar código de terceros. Hacerlo en el anfitrión
// —aunque sea en un chroot, como hace el empaquetador de imágenes— saca esa
// ejecución fuera de la frontera que kindling levanta, que es justo la razón de
// ser del proyecto. Aquí ocurre dentro de una microVM que se destruye a
// continuación: si un paquete trae sorpresas, se lleva por delante una máquina
// de dos segundos de vida.
//
// La microVM se monta con el volumen en ESCRITURA, lo que implica exclusividad:
// mientras se puebla, nadie más puede montarlo, ni siquiera para leer. Eso lo
// impone resolveVolumes y es deliberado — actualizar la biblioteca bajo los pies
// de quien la está leyendo es la corrupción de siempre.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// populateTimeout acota lo que puede tardar una instalación.
//
// Generoso a propósito: un `npm install` de un árbol grande sobre una microVM de
// un vCPU pasa de varios minutos sin estar colgado, y cortarlo dejaría el
// volumen a medio poblar — que es peor que no haber empezado, porque parece
// completo.
const populateTimeout = 20 * time.Minute

// PopulateVolume arranca una microVM de un solo uso con el volumen montado en
// escritura, ejecuta el comando dentro y la destruye.
//
// La máquina se destruye SIEMPRE, también si el comando falla o si se cancela el
// contexto: una microVM huérfana retendría el volumen en exclusiva y nadie
// podría ni leerlo, y el mensaje de ese fallo no señalaría a esta función por
// ningún lado.
func (m *Manager) PopulateVolume(ctx context.Context, req api.PopulateRequest) (*api.PopulateResult, error) {
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("missing command to execute")
	}
	mount := req.Mount
	if mount == "" {
		mount = defaultVolumeMount
	}
	image := req.Image
	if image == "" {
		return nil, fmt.Errorf("an image with the installer is required: " +
			"build it with `kling images toolchain`")
	}
	memMiB := req.MemMiB
	if memMiB <= 0 {
		memMiB = 1024
	}

	mc, err := m.Run(ctx, api.RunRequest{
		Name:   "populate-" + req.Volume,
		Image:  image,
		VCPUs:  1,
		MemMiB: memMiB,
		// Con salida a internet: instalar paquetes es, por definición,
		// descargarlos.
		Egress: "internet",
		Volumes: []api.VolumeAttachment{
			{Name: req.Volume, Mount: mount},
		},
		AllowExec: true,
	})
	if err != nil {
		return nil, err
	}
	// La máquina se destruye pase lo que pase: una microVM huérfana retendría el
	// volumen en exclusiva y nadie podría ni leerlo, y el mensaje de ESE fallo no
	// señalaría a esta función por ningún lado.
	defer func() { _ = m.Remove(mc.ID) }()

	base := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort))
	if err := waitGuest(ctx, base, 60*time.Second); err != nil {
		return nil, fmt.Errorf("installation microVM did not start listening: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, populateTimeout)
	defer cancel()
	out, err := ejecutarEnInvitado(runCtx, base, req.Cmd)
	if err != nil {
		return nil, err
	}

	// El vaciado a disco se lo pide Remove() al matar, pero aquí se pide antes
	// de mirar el tamaño: si no, el "en disco" que se informa sería el de antes
	// de que la caché del invitado llegara al fichero.
	m.flushVolume(mc)

	res := &api.PopulateResult{ExitCode: out.ExitCode, Output: out.Output, Machine: mc.ID}
	if v, err := m.statVolume(req.Volume); err == nil {
		res.UsedMiB = v.UsedBytes >> 20
	}
	return res, nil
}

// waitGuest espera a que el puente conteste dentro de la microVM.
func waitGuest(ctx context.Context, base string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	cli := &http.Client{Timeout: 2 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := cli.Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// ejecutarEnInvitado corre cmd dentro de la microVM y devuelve su código y su
// salida, stdout y stderr mezclados en el orden en que llegaron.
//
// Usa /exec/stream, la ruta de los sandboxes, y solo si el agente no la conoce
// (imágenes anteriores a v0.7) cae a /exec, la de siempre. Así poblar un volumen
// hereda lo que tiene la de streaming —plazo que mata al grupo, topes de
// salida— sin dejar de funcionar con imágenes viejas.
func ejecutarEnInvitado(ctx context.Context, base string, cmd []string) (poblado, error) {
	body, _ := json.Marshal(api.ExecRequest{Cmd: cmd, TimeoutSeconds: int(populateTimeout / time.Second)})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exec/stream", bytes.NewReader(body))
	if err != nil {
		return poblado{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	// Sin plazo de cabeceras: el agente contesta al empezar, pero una
	// instalación larga no es un invitado colgado. El límite es el contexto.
	resp, err := (&http.Client{}).Do(hreq)
	if err != nil {
		return poblado{}, fmt.Errorf("executing inside the microVM: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ejecutarLegado(ctx, base, cmd)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return poblado{}, fmt.Errorf("guest agent: %s", strings.TrimSpace(string(msg)))
	}

	var salida bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev api.ExecEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch {
		case ev.Error != "":
			return poblado{}, fmt.Errorf("executing inside the microVM: %s", ev.Error)
		case ev.Exit != nil:
			return poblado{ExitCode: *ev.Exit, Output: salida.String()}, nil
		case len(ev.Data) > 0 && salida.Len() < api.ExecDefaultOutput:
			salida.Write(ev.Data)
		}
	}
	return poblado{}, fmt.Errorf("the guest stopped answering before the installation finished")
}

// poblado es el resultado de una ejecución de instalación.
type poblado struct {
	ExitCode int
	Output   string
}

// ejecutarLegado es la ruta /exec de los agentes anteriores a v0.7: la salida
// llega entera, mezclada, al final.
func ejecutarLegado(ctx context.Context, base string, cmd []string) (poblado, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exec", bytes.NewReader(body))
	if err != nil {
		return poblado{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(hreq)
	if err != nil {
		return poblado{}, fmt.Errorf("executing inside the microVM: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return poblado{}, fmt.Errorf("unreadable response from guest: %w", err)
	}
	return poblado{ExitCode: out.ExitCode, Output: out.Output}, nil
}
