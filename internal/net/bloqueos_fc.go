//go:build !darwin

package net

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// nftTabla es la tabla de nftables (dentro del netns de la máquina) cuyo set
// recoge las conexiones nuevas que la política de salida va a tirar.
const nftTabla = "kl_bloq"

// reglasRegistroIP es el script de nft que apunta, ANTES de que el DROP de
// iptables las tire (prioridad -10, la de iptables es 0), las conexiones nuevas
// del invitado: IP . protocolo . puerto, con un contador por elemento. Excluye
// lo que va a la IP del host en el veth (DNS del resolver, proxies): eso no
// sale. El set está acotado (size) y caduca solo (timeout), así que el
// kernel hace de tope; si se llena, la regla simplemente deja de apuntar.
func (n *Net) reglasRegistroIP() string {
	return fmt.Sprintf(`table ip %[1]s {
	set d { type ipv4_addr . inet_proto . inet_service; flags dynamic,timeout; size %[2]d; timeout 1h; counter; }
	chain f { type filter hook forward priority -10; policy accept;
		iifname "%[3]s" ct state new meta l4proto { tcp, udp } ip daddr != %[4]s update @d { ip daddr . meta l4proto . th dport counter }
	}
}
`, nftTabla, bloqueosMaxDestinos*2, TapName, n.HostIP)
}

// instalarRegistroIP monta el registro de IP:puerto en el netns. Es de mejor
// esfuerzo: sin nft (o sin nf_tables en el kernel) el aviso se queda con los
// nombres y el contador, y la máquina arranca igual.
//
// Va en segundo plano: nft tarda ~17 ms en el lab (medido) y el invitado no
// emite tráfico hasta bastante después de que Setup devuelva; esperarlo
// añadiría esos ms a cada arranque. Si el netns se desmonta antes de que
// acabe, nft falla y no pasa nada.
func (n *Net) instalarRegistroIP() {
	if instalarRegistroHook != nil {
		instalarRegistroHook(n)
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return
	}
	script := n.reglasRegistroIP()
	ns := n.NS
	registrosEnVuelo.Add(1)
	go func() {
		defer registrosEnVuelo.Done()
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		_ = ejecutarEnNetns(ns, cmd) // setns, sin `ip netns exec`
	}()
}

// instalarRegistroHook avisa de cada instalación del registro (tests).
var instalarRegistroHook func(*Net)

// registroEnRanura: netns de ranuras que ya traen el registro de none
// instalado por MontarRanura (la tabla solo depende de HostIP y del tap). Lo
// consume arrancarRegistroNone (none: no lo repite) o aplicarSobreRanura (otro
// modo: lo retira, porque un registro solo tiene sentido donde se corta).
var registroEnRanura sync.Map // ns -> struct{}

// retirarRegistroDeRanura quita el registro de una ranura que se entrega con
// un modo que no es none. En segundo plano, como la instalación.
func (n *Net) retirarRegistroDeRanura() {
	if _, ok := registroEnRanura.LoadAndDelete(n.NS); !ok {
		return
	}
	ns := n.NS
	registrosEnVuelo.Add(1)
	go func() {
		defer registrosEnVuelo.Done()
		cmd := exec.Command("nft", "delete", "table", "ip", nftTabla)
		_ = ejecutarEnNetns(ns, cmd)
	}()
}

// registrosEnVuelo cuenta los nft de instalarRegistroIP que siguen corriendo:
// los tests que cambian opsNetns esperan a que acaben (si no, leen la
// variable mientras el test la sustituye).
var registrosEnVuelo sync.WaitGroup

// destinosIP lee el set de nft del netns de n.
func destinosIP(n *Net) []Destino {
	if _, err := exec.LookPath("nft"); err != nil {
		return nil
	}
	var buf bytes.Buffer
	cmd := exec.Command("nft", "-j", "list", "set", "ip", nftTabla, "d")
	cmd.Stdout = &buf
	if err := ejecutarEnNetns(n.NS, cmd); err != nil {
		return nil
	}
	return parseSetNft(buf.Bytes())
}

// parseSetNft extrae los destinos de la salida JSON de `nft -j list set`.
func parseSetNft(b []byte) []Destino {
	var doc struct {
		Nftables []struct {
			Set struct {
				Elem []struct {
					Elem struct {
						Val struct {
							Concat []json.RawMessage `json:"concat"`
						} `json:"val"`
						Counter struct {
							Packets int `json:"packets"`
						} `json:"counter"`
					} `json:"elem"`
				} `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	var out []Destino
	for _, item := range doc.Nftables {
		for _, e := range item.Set.Elem {
			c := e.Elem.Val.Concat
			if len(c) != 3 {
				continue
			}
			var ip, proto string
			var puerto int
			if json.Unmarshal(c[0], &ip) != nil || json.Unmarshal(c[1], &proto) != nil {
				continue
			}
			// El puerto sale como número; el protocolo, como nombre.
			if json.Unmarshal(c[2], &puerto) != nil {
				var s string
				if json.Unmarshal(c[2], &s) != nil {
					continue
				}
				p, err := strconv.Atoi(s)
				if err != nil {
					continue
				}
				puerto = p
			}
			cnt := e.Elem.Counter.Packets
			if cnt < 1 {
				cnt = 1
			}
			out = append(out, Destino{Dest: formatoDestino(ip, proto, puerto), Kind: "ip", Count: cnt})
		}
	}
	return out
}

// ReanudarResolver rearranca el resolver de una máquina none tras un reinicio
// del daemon (el netns y sus reglas sobreviven; la goroutine no).
func (n *Net) ReanudarResolver(e Egress) error {
	if e != EgressNone {
		return nil
	}
	return startDNSResolverModo(n, nil, modoSoloGrafo)
}
