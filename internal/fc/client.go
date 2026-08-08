// Package fc habla con la API REST de Firecracker por su socket Unix.
//
// Deliberadamente sin SDK: la superficie que necesitamos son seis llamadas y así
// no arrastramos deriva de versiones con un binario que actualizamos aparte.
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
