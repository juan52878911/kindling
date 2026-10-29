package api

// Puertos publicados por el daemon (internal/publicar): 127.0.0.1:Host del
// host lleva al puerto Guest de la máquina, como `docker run -p`. Los publica
// el DAEMON, no el CLI: sobreviven a que el CLI acabe (`compose up -d`) y al
// reinicio del daemon (se guardan en su raíz), y se quitan con la máquina.
// Nunca en otra interfaz que 127.0.0.1.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// CapabilityPublish es la capacidad de GET /info que dice que el daemon
// publica puertos (POST/DELETE /machines/{ref}/publish, GET /publish y
// RunRequest.Publish).
const CapabilityPublish = "publish"

// LabelIdle = "keep" excluye una máquina de daemon.idle_freeze: con puertos
// publicados y sin conexiones durante el plazo, el daemon la congela, salvo
// que lleve esta etiqueta.
const LabelIdle = "kling.idle"

// Protocolos de PortPublish.
const (
	ProtoTCP = "tcp"
	ProtoUDP = "udp"
)

// PortPublish es un puerto publicado: 127.0.0.1:Host → Guest de la máquina.
// Host 0 pide uno libre al azar (el `ports: "80"` de Docker); la respuesta
// dice el que tocó.
type PortPublish struct {
	Host  int    `json:"host"`
	Guest int    `json:"guest"`
	Proto string `json:"proto,omitempty"` // tcp (vacío) o udp
}

// ProtoOr es el protocolo, tcp si no se dio.
func (p PortPublish) ProtoOr() string {
	if p.Proto == "" {
		return ProtoTCP
	}
	return p.Proto
}

// String es 127.0.0.1:H->G[/udp], como lo enseña kling ps.
func (p PortPublish) String() string {
	s := "127.0.0.1:" + strconv.Itoa(p.Host) + "->" + strconv.Itoa(p.Guest)
	if p.ProtoOr() != ProtoTCP {
		s += "/" + p.Proto
	}
	return s
}

// Validate comprueba un puerto pedido.
func (p PortPublish) Validate() error {
	if p.Host < 0 || p.Host > 65535 {
		return fmt.Errorf("host port %d out of range", p.Host)
	}
	if p.Guest < 1 || p.Guest > 65535 {
		return fmt.Errorf("guest port %d out of range", p.Guest)
	}
	switch p.Proto {
	case "", ProtoTCP, ProtoUDP:
	default:
		return fmt.Errorf("protocol %q: use tcp or udp", p.Proto)
	}
	if p.Guest == GuestPort && p.ProtoOr() == ProtoTCP {
		return fmt.Errorf("port %d is the guest agent; it is never published", p.Guest)
	}
	return nil
}

// PublishRequest es el cuerpo de POST /machines/{ref}/publish.
type PublishRequest struct {
	Ports []PortPublish `json:"ports"`
}

// Publication son los puertos que el daemon publica para una máquina.
type Publication struct {
	Machine string        `json:"machine"`
	Name    string        `json:"name,omitempty"`
	Ports   []PortPublish `json:"ports"`
}

// FormatPorts es la columna PORTS de kling ps.
func FormatPorts(ps []PortPublish) string {
	var l []string
	for _, p := range ps {
		l = append(l, p.String())
	}
	return strings.Join(l, ",")
}

// Publish publica puertos de la máquina ref (se suman a los que ya tuviera;
// un Host ya publicado para ella se sustituye). Todos o ninguno: un puerto
// ocupado falla sin publicar los demás.
func (c *Client) Publish(ctx context.Context, ref string, ports []PortPublish) (*Publication, error) {
	var p Publication
	return &p, c.do(ctx, http.MethodPost, "/machines/"+ref+"/publish", PublishRequest{Ports: ports}, &p)
}

// Unpublish deja de publicar los puertos del host hosts de la máquina ref
// (ninguno: todos).
func (c *Client) Unpublish(ctx context.Context, ref string, hosts ...int) error {
	path := "/machines/" + ref + "/publish"
	if len(hosts) > 0 {
		var l []string
		for _, h := range hosts {
			l = append(l, strconv.Itoa(h))
		}
		path += "?host=" + strings.Join(l, ",")
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Publications son todas las publicaciones del daemon.
func (c *Client) Publications(ctx context.Context) ([]Publication, error) {
	var out []Publication
	return out, c.do(ctx, http.MethodGet, "/publish", nil, &out)
}
