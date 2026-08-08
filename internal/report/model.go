package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// El informe no dibuja: describe. Go emite un modelo plano y el navegador lo
// convierte en el árbol que toque según la vista y la profundidad elegidas.
// Así una sola estructura sirve para la topología, las capas internas, el
// catálogo MCP y la red, sin generar cuatro SVG distintos aquí.

type Model struct {
	Host     Host      `json:"host"`
	Services []Service `json:"services"`
}

type Host struct {
	Endpoint    string `json:"endpoint"`
	KVM         bool   `json:"kvm"`
	Firecracker string `json:"firecracker"`
	Generated   string `json:"generated"`
	Memory      string `json:"memory"` // servicio de memoria activo, si lo hay
	Running     int    `json:"running"`
	Warm        int    `json:"warm"`
	Ready       int    `json:"ready"`
	External    int    `json:"external"`
	RAMShared   string `json:"ramShared"`
	DiskOwn     string `json:"diskOwn"`
}

type Service struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`  // microvm | externo | roto
	State     string   `json:"state"` // atendiendo | dormido | listo | bloqueado
	Latency   string   `json:"latency"`
	Ephemeral bool     `json:"ephemeral"`
	Snapshot  string   `json:"snapshot"`
	Image     string   `json:"image"`
	RAMShared string   `json:"ramShared"`
	URL       string   `json:"url"`
	VCPUs     int      `json:"vcpus"`
	MemMiB    int      `json:"memMiB"`
	Tools     []Tool   `json:"tools"`
	Machines  []Mach   `json:"machines"`
	Flow      []string `json:"flow"`
	Store     string   `json:"store"`
	StoreOK   bool     `json:"storeOk"`
	Writers   []string `json:"writers"`
}

type Tool struct {
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Writes bool   `json:"writes"`
}

type Mach struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	State  string            `json:"state"`
	IP     string            `json:"ip"`
	Egress string            `json:"egress"`
	LastOp string            `json:"lastOp"`
	Age    string            `json:"age"`
	VCPUs  int               `json:"vcpus"`
	MemMiB int               `json:"memMiB"`
	Disk   string            `json:"disk"`
	Labels map[string]string `json:"labels"`
}

func BuildModel(info *api.Info, groups []Group, endpoint string, now time.Time, memService string) Model {
	m := Model{Host: Host{
		Endpoint:  endpoint,
		Generated: now.Format("2006-01-02 15:04"),
		Memory:    memService,
	}}
	if info != nil {
		m.Host.KVM = info.KVM
		m.Host.Firecracker = strings.TrimSpace(info.Firecrack)
	}

	var ramShared, diskOwn int64
	for _, g := range groups {
		ramShared += g.RAMShared
		s := buildService(g, memService)
		for _, mm := range g.Machines {
			diskOwn += mm.DiskBytes
		}
		switch {
		case s.Kind == "externo":
			m.Host.External++
		case s.State == "atendiendo":
			m.Host.Running++
		case s.State == "dormido":
			m.Host.Warm++
		default:
			m.Host.Ready++
		}
		m.Services = append(m.Services, s)
	}
	m.Host.RAMShared = human(ramShared)
	m.Host.DiskOwn = human(diskOwn)
	if m.Services == nil {
		m.Services = []Service{}
	}
	return m
}

func buildService(g Group, memService string) Service {
	r := g.Readiness()
	s := Service{
		Name: g.Service, Latency: r.Latency, Flow: g.Flow(),
		Tools: buildTools(g), Machines: buildMachines(g),
		Writers: g.writers(),
	}

	switch {
	case g.Link != nil:
		s.Kind, s.State = "externo", "listo"
		s.URL = g.Link.URL
		s.Store, s.StoreOK = "propio, fuera de kindling", true
	case r.Blocked != "":
		s.Kind, s.State = "roto", "bloqueado"
		s.Latency = r.Blocked
	default:
		s.Kind = "microvm"
		s.Ephemeral = g.Ephemeral()
		s.Snapshot = g.Snapshot.Name
		s.Image = g.Snapshot.Image
		s.RAMShared = human(g.Snapshot.MemBytes)
		s.VCPUs, s.MemMiB = g.Snapshot.VCPUs, g.Snapshot.MemMiB
		switch {
		case r.Live > 0:
			s.State = "atendiendo"
		case r.Warm > 0:
			s.State = "dormido"
		default:
			s.State = "listo"
		}
		if s.Ephemeral {
			s.Store, s.StoreOK = "se pierde al terminar la llamada", false
		} else {
			s.Store, s.StoreOK = "vive mientras viva la instancia", false
		}
	}

	if len(s.Writers) == 0 && s.Kind != "roto" {
		s.Store, s.StoreOK = "no escribe nada", true
	} else if !s.StoreOK && memService != "" {
		s.Store += " → guárdalo en " + memService
	}
	return s
}

func buildTools(g Group) []Tool {
	var src []api.ToolSpec
	switch {
	case g.Link != nil:
		src = g.Link.Tools
	case g.Snapshot != nil:
		src = g.Snapshot.Tools
	}
	writes := map[string]bool{}
	for _, w := range g.writers() {
		writes[w] = true
	}
	out := make([]Tool, 0, len(src))
	for _, t := range src {
		out = append(out, Tool{Name: t.Name, Desc: firstLine(t.Description), Writes: writes[t.Name]})
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 90 {
		s = string([]rune(s)[:89]) + "…"
	}
	return s
}

func buildMachines(g Group) []Mach {
	out := make([]Mach, 0, len(g.Machines))
	now := time.Now()
	for _, m := range g.Machines {
		ip := m.IP
		if m.State != api.StateRunning || ip == "" {
			ip = "—"
		}
		out = append(out, Mach{
			ID: m.ID, Name: m.Name, State: string(m.State), IP: ip,
			Egress: m.Egress, LastOp: lastOp(m), Age: age(m.CreatedAt, now),
			VCPUs: m.VCPUs, MemMiB: m.MemMiB, Disk: human(m.DiskBytes),
			Labels: m.Labels,
		})
	}
	return out
}

// modelJSON deja el modelo listo para incrustarlo en un <script>. Un "</script>"
// dentro de la descripción de una herramienta cerraría la etiqueta antes de
// tiempo, así que se escapa el "<".
func modelJSON(m Model) string {
	b, err := json.Marshal(m)
	if err != nil {
		return `{"host":{},"services":[]}`
	}
	return strings.ReplaceAll(string(b), "<", `<`)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
