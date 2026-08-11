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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/internal/api"
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
		return nil, fmt.Errorf("falta el comando a ejecutar")
	}
	mount := req.Mount
	if mount == "" {
		mount = defaultVolumeMount
	}
	image := req.Image
	if image == "" {
		return nil, fmt.Errorf("hace falta una imagen que traiga el instalador: " +
			"-image con una que tenga npm o pip")
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
	// Pase lo que pase por debajo. Con context.WithoutCancel porque el contexto
	// de la petición HTTP puede estar ya cancelado justo cuando más falta hace
	// limpiar: si el cliente cortó, la máquina sigue viva y reteniendo el
	// volumen.
	defer func() { _ = m.Remove(mc.ID) }()

	base := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort))
	if err := waitGuest(ctx, base, 60*time.Second); err != nil {
		return nil, fmt.Errorf("la microVM de instalación no empezó a escuchar: %w", err)
	}

	body, _ := json.Marshal(map[string]any{"cmd": req.Cmd})
	runCtx, cancel := context.WithTimeout(ctx, populateTimeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(runCtx, http.MethodPost, base+"/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")

	// Sin plazo de cabeceras en el cliente: el invitado no responde hasta que el
	// comando termina, y una instalación larga no es un invitado colgado. El
	// límite real es populateTimeout, arriba.
	cli := &http.Client{}
	resp, err := cli.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("ejecutando dentro de la microVM: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("respuesta ilegible del invitado: %w", err)
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
