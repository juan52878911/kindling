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
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
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

// LoadSnapshot restaura y reanuda. Es la operación de ~30 ms sobre la que se
// sostiene todo el proyecto.
func (c *Client) LoadSnapshot(ctx context.Context, snapPath, memPath string) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": snapPath,
		"mem_backend":   map[string]string{"backend_path": memPath, "backend_type": "File"},
		"resume_vm":     true,
	})
}
