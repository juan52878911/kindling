package net

// Destinos que la política de salida de una microVM le ha cortado, para
// avisar al usuario de POR QUÉ falló una imagen que descarga algo al arrancar
// (en vez de un error de red genérico). Solo se guarda el destino y cuántas
// veces se intentó: ni cuerpo, ni cabeceras, ni qué se mandaba.
//
// Dos fuentes, ninguna componente nuevo:
//   - Nombres: el resolver del netns (dnsresolver.go) contesta NXDOMAIN (none)
//     o REFUSED (allowlist) y apunta el nombre aquí.
//   - IP:puerto: un set dinámico de nftables en el propio netns (bloqueos_fc.go)
//     que el kernel rellena con cada conexión nueva que el DROP va a tirar.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	// bloqueosMaxNombres acota cuántos nombres distintos se recuerdan por
	// máquina: un invitado que consulte nombres aleatorios no crece sin fin.
	bloqueosMaxNombres = 64
	// bloqueosMaxDestinos acota lo que Blocked devuelve (nombres + IP:puerto).
	bloqueosMaxDestinos = 32
)

// Destino es un destino bloqueado: un dominio (Kind "dns") o una IP:puerto
// con su protocolo (Kind "ip"). Count son los intentos vistos.
type Destino struct {
	Dest  string `json:"dest"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// registroBloqueos son los nombres cortados por el resolver de un netns.
type registroBloqueos struct {
	mu      sync.Mutex
	nombres map[string]int
}

// anotar apunta una consulta de name. Solo la A cuenta (los clientes piden A y
// AAAA a la vez: contar las dos duplicaría); una AAAA suelta deja el nombre
// con 0, que Blocked lee como 1.
func (b *registroBloqueos) anotar(name string, qtype uint16) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" || strings.HasSuffix(name, ".arpa") {
		return
	}
	if qtype != 1 && qtype != 28 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nombres == nil {
		b.nombres = map[string]int{}
	}
	if _, ok := b.nombres[name]; !ok && len(b.nombres) >= bloqueosMaxNombres {
		return
	}
	if qtype == 1 {
		b.nombres[name]++
	} else if _, ok := b.nombres[name]; !ok {
		b.nombres[name] = 0
	}
}

func (b *registroBloqueos) destinos() []Destino {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Destino
	for n, c := range b.nombres {
		if c < 1 {
			c = 1
		}
		out = append(out, Destino{Dest: n, Kind: "dns", Count: c})
	}
	return out
}

// juntarDestinos mezcla nombres e IP:puerto, ordena por intentos (y por texto
// para que sea estable) y corta en bloqueosMaxDestinos.
func juntarDestinos(grupos ...[]Destino) []Destino {
	var out []Destino
	for _, g := range grupos {
		out = append(out, g...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Dest < out[j].Dest
	})
	if len(out) > bloqueosMaxDestinos {
		out = out[:bloqueosMaxDestinos]
	}
	return out
}

// Blocked devuelve los destinos que la salida de la máquina n ha cortado
// hasta ahora. Vacío si no hay nada, o si este sistema no puede saberlo.
func Blocked(n *Net) []Destino {
	var nombres []Destino
	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r != nil {
		nombres = r.bloq.destinos()
	}
	return juntarDestinos(nombres, destinosIP(n))
}

// formatoDestino pinta un elemento del set de nftables: IP . proto . puerto.
func formatoDestino(ip, proto string, puerto int) string {
	if proto == "tcp" {
		return fmt.Sprintf("%s:%d", ip, puerto)
	}
	return fmt.Sprintf("%s:%d/%s", ip, puerto, proto)
}
