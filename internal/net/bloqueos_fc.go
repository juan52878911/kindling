//go:build !darwin

package net

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
func (n *Net) instalarRegistroIP() {
	if _, err := exec.LookPath("nft"); err != nil {
		return
	}
	cmd := exec.Command("ip", "netns", "exec", n.NS, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(n.reglasRegistroIP())
	_ = cmd.Run()
}

// destinosIP lee el set de nft del netns de n.
func destinosIP(n *Net) []Destino {
	if _, err := exec.LookPath("nft"); err != nil {
		return nil
	}
	out, err := exec.Command("ip", "netns", "exec", n.NS, "nft", "-j", "list", "set", "ip", nftTabla, "d").Output()
	if err != nil {
		return nil
	}
	return parseSetNft(out)
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
