// Package fc habla con la API REST de Firecracker por su socket Unix.
//
// Deliberadamente sin SDK: la superficie que necesitamos es un puñado de
// llamadas y así no arrastramos deriva de versiones con un binario que
// actualizamos aparte.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Client apunta al socket de una instancia concreta de Firecracker.
type Client struct {
	http *http.Client
}

func New(socketPath string) *Client {
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}}
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, e.FaultMessage)
	}
	return nil
}

// Ping comprueba que el socket responde; se usa para esperar el arranque.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type Drive struct {
	DriveID      string       `json:"drive_id"`
	PathOnHost   string       `json:"path_on_host"`
	IsRootDevice bool         `json:"is_root_device"`
	IsReadOnly   bool         `json:"is_read_only"`
	RateLimiter  *RateLimiter `json:"rate_limiter,omitempty"`
}

// RateLimiter acota el caudal de un dispositivo. Sin esto, una sola microVM
// hostil puede saturar el disco o la red del host y degradar a todas las demás.
type RateLimiter struct {
	Bandwidth *TokenBucket `json:"bandwidth,omitempty"`
	Ops       *TokenBucket `json:"ops,omitempty"`
}

type TokenBucket struct {
	Size       int64 `json:"size"`        // tokens por intervalo
	RefillTime int64 `json:"refill_time"` // ms para rellenar
}

// Limit construye un limitador de caudal en bytes por segundo.
func Limit(bytesPerSec int64) *RateLimiter {
	return &RateLimiter{Bandwidth: &TokenBucket{Size: bytesPerSec, RefillTime: 1000}}
}

type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type NetworkInterface struct {
	IfaceID     string       `json:"iface_id"`
	HostDevName string       `json:"host_dev_name"`
	GuestMAC    string       `json:"guest_mac,omitempty"`
	RxLimiter   *RateLimiter `json:"rx_rate_limiter,omitempty"`
	TxLimiter   *RateLimiter `json:"tx_rate_limiter,omitempty"`
}

func (c *Client) SetBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}

func (c *Client) SetDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
}

func (c *Client) SetMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}

func (c *Client) SetNetwork(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n)
}

// PatchDrive reapunta un disco ya configurado a otro fichero del host.
//
// Es la pieza que hace posible el snapshot dorado: el snapshot lleva grabadas
// las rutas de los discos, así que al restaurar hay que reapuntar el overlay al
// de esta instancia antes de reanudar. El fichero nuevo debe ser una COPIA del
// que se congeló: el invitado despierta con su estado de montaje en memoria, y
// darle un disco con otro contenido lo corrompería.
func (c *Client) PatchDrive(ctx context.Context, driveID, pathOnHost string) error {
	return c.do(ctx, http.MethodPatch, "/drives/"+driveID, map[string]string{
		"drive_id":     driveID,
		"path_on_host": pathOnHost,
	})
}

// SetEntropy añade un virtio-rng al invitado.
//
// Es la contramedida al problema documentado por AWS: las instancias que
// restauran del mismo snapshot CLONAN el estado del generador de aleatoriedad.
// Con esto y con CONFIG_VMGENID en el kernel invitado, el kernel detecta que
// viene de un snapshot y vuelve a sembrar su pool. Sin ello, dos herramientas
// distintas podrían generar las mismas claves TLS.
func (c *Client) SetEntropy(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/entropy", map[string]any{})
}

func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// Snapshot congela la máquina en disco. Requiere haberla pausado antes.
func (c *Client) Snapshot(ctx context.Context, snapPath, memPath string) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": snapPath,
		"mem_file_path": memPath,
	})
}

// LoadSnapshot restaura desde un snapshot. Es la operación de ~30 ms sobre la
// que se sostiene todo el proyecto.
//
// El backend "File" hace que Firecracker MAPEE el fichero de memoria en vez de
// reservar memoria anónima. De ahí salen dos propiedades decisivas: las páginas
// entran bajo demanda, y lo residente es page cache — desalojable sin swap y
// COMPARTIDO entre las instancias que restauren del mismo fichero.
//
// resume=false deja la microVM pausada para poder reapuntar discos antes de
// arrancarla.
func (c *Client) LoadSnapshot(ctx context.Context, snapPath, memPath string, resume bool) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": snapPath,
		"mem_backend":   map[string]string{"backend_path": memPath, "backend_type": "File"},
		"resume_vm":     resume,
	})
}

// SetBalloon configura el globo de memoria (virtio-balloon) del invitado.
//
// TIENE que hacerse ANTES de Start: Firecracker no admite añadir el globo en
// caliente. Por eso queda grabado en el snapshot dorado y las copias restauradas
// lo heredan sin más. Al arrancar se deja a 0 (no reclama nada); lo que habilita
// es poder APRETARLO luego para devolver RAM al host entre sesiones.
//
// deflateOnOOM deja que el invitado recupere sus páginas si se queda corto, en
// vez de morir por el OOM killer del propio invitado. statsPollingSec>0 activa
// las estadísticas del globo —imprescindibles para saber cuánta memoria libre
// tiene el invitado antes de apretarlo—.
func (c *Client) SetBalloon(ctx context.Context, amountMiB int, deflateOnOOM bool, statsPollingSec int) error {
	return c.do(ctx, http.MethodPut, "/balloon", map[string]any{
		"amount_mib":               amountMiB,
		"deflate_on_oom":           deflateOnOOM,
		"stats_polling_interval_s": statsPollingSec,
	})
}

// PatchBalloon cambia el tamaño del globo en caliente.
//
// Inflarlo hace que el driver del invitado entregue páginas libres al host, que
// Firecracker devuelve con madvise(DONTNEED): así cae el RSS del proceso en el
// anfitrión sin congelar la microVM. Desinflarlo a 0 deja que el invitado vuelva
// a usar esa memoria —las páginas reentran perezosas como ceros—.
func (c *Client) PatchBalloon(ctx context.Context, amountMiB int) error {
	return c.do(ctx, http.MethodPatch, "/balloon", map[string]int{"amount_mib": amountMiB})
}

// BalloonStats son las estadísticas del globo. Los campos de memoria van en
// BYTES. Solo existen si el globo se configuró con statsPollingSec>0.
type BalloonStats struct {
	TargetMiB       int   `json:"target_mib"`
	ActualMiB       int   `json:"actual_mib"`
	FreeMemory      int64 `json:"free_memory"`
	AvailableMemory int64 `json:"available_memory"`
	TotalMemory     int64 `json:"total_memory"`
}

// SetMMDS configura el metadata service (MMDS) de Firecracker: qué versión y por
// qué interfaces lo sirve. TIENE que hacerse ANTES de Start, como el balloon:
// Firecracker no admite añadir MMDS en caliente. Por eso queda grabado en el
// snapshot dorado y las copias restauradas lo heredan.
//
// MODELO DE AMENAZA. MMDS es el canal por el que se inyecta un secreto de sesión
// —un token, una credencial— en una microVM YA VIVA, sin hornearlo en la imagen
// compartida ni en el snapshot dorado. El invitado es hostil, pero el secreto es
// SUYO (de esa sesión); lo que se busca es que no acabe grabado en un artefacto
// que comparten todas las instancias.
//
// Versión V2 a propósito: exige que el invitado pida primero un token de sesión
// (PUT .../api/token) y lo presente en cada lectura (cabecera X-metadata-token).
// V1 deja leer sin token, y como el gateway reenvía peticiones a los invitados,
// un canal de metadatos legible sin credencial es una fuga esperando a ocurrir.
// La 169.254.169.254 es la link-local estándar del metadata service.
func (c *Client) SetMMDS(ctx context.Context, ifaceIDs []string) error {
	return c.do(ctx, http.MethodPut, "/mmds/config", map[string]any{
		"version":            "V2",
		"ipv4_address":       "169.254.169.254",
		"network_interfaces": ifaceIDs,
	})
}

// PutMMDSData escribe el store JSON del MMDS. Es lo que el invitado lee luego por
// 169.254.169.254. A diferencia de SetMMDS (config, pre-boot), esto se hace sobre
// una microVM VIVA: es la inyección del secreto de sesión.
//
// El store es un único documento JSON que PISA por completo al anterior (PUT, no
// merge). El esquema lo define kindling (ver bridge): un objeto con "env" (comunes
// a todas las sesiones) y "sessions" keyed por Mcp-Session-Id.
func (c *Client) PutMMDSData(ctx context.Context, data any) error {
	return c.do(ctx, http.MethodPut, "/mmds", data)
}

// BalloonStats lee las estadísticas del globo del invitado. A diferencia de do(),
// necesita el cuerpo de la respuesta, así que hace la petición y la decodifica
// aquí. Devuelve error si el globo no está configurado (imagen anterior al
// balloon) o si el invitado aún no ha reportado.
func (c *Client) BalloonStats(ctx context.Context) (*BalloonStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/balloon/statistics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("firecracker GET /balloon/statistics: %s: %s", resp.Status, e.FaultMessage)
	}
	var s BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}
