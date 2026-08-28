package main

// Lo que el invitado sabe de su propia resolucion de nombres.
//
// Existe porque un fallo de DNS dentro de una microVM no se ve desde fuera: el
// servidor MCP arranca, responde tools/list, y solo falla cuando de verdad
// intenta salir. El caso que lo motiva: las imagenes heredaban el resolv.conf
// del anfitrion, que con systemd-resolved dice 127.0.0.53 —inexistente dentro—
// y cuyo resolver real suele ser una IP privada que el cortafuegos bloquea a
// proposito. Rompia la navegacion de un servicio que figuraba sano.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// dnsInfo se devuelve tal cual por /dns.
type dnsInfo struct {
	Nameservers []string `json:"nameservers"`
	Resuelve    bool     `json:"resuelve"`
	Probado     string   `json:"probado,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// nameserversDe saca las direcciones de un resolv.conf.
func nameserversDe(texto string) []string {
	var out []string
	for _, linea := range strings.Split(texto, "\n") {
		linea = strings.TrimSpace(linea)
		if linea == "" || strings.HasPrefix(linea, "#") || strings.HasPrefix(linea, ";") {
			continue
		}
		campos := strings.Fields(linea)
		if len(campos) >= 2 && campos[0] == "nameserver" {
			out = append(out, campos[1])
		}
	}
	return out
}

// diagnosticoDNS lee la configuracion y prueba una resolucion.
//
// Las dos mitades son distintas a proposito: la configuracion se comprueba sin
// tocar la red —y es la que cazaba el fallo real—, y la resolucion es lo que
// dice si ademas hay salida.
func diagnosticoDNS(ctx context.Context, dominio string) dnsInfo {
	info := dnsInfo{Probado: dominio}
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		info.Error = "no /etc/resolv.conf: " + err.Error()
		return info
	}
	info.Nameservers = nameserversDe(string(b))
	if len(info.Nameservers) == 0 {
		info.Error = "/etc/resolv.conf has no nameserver line"
		return info
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, dominio); err != nil {
		info.Error = err.Error()
		return info
	}
	info.Resuelve = true
	return info
}

func (b *bridge) handleDNS(w http.ResponseWriter, r *http.Request) {
	dominio := r.URL.Query().Get("host")
	if dominio == "" {
		dominio = "example.com"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(diagnosticoDNS(r.Context(), dominio))
}
