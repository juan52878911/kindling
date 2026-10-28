package daemon

import (
	"fmt"
	"net/http"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// handleEgress sirve los destinos que la política de salida de la máquina le
// cortó (internal/net/bloqueos.go): dominio si lo hay, si no IP:puerto, con
// los intentos vistos. Con egress internet no se corta nada y sale vacío.
func (s *Server) handleEgress(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	mc, ok := s.mgr.Get(ref)
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("machine %q does not exist", ref))
		return
	}
	out := api.EgressBlocked{Egress: mc.Egress}
	if out.Egress == "" {
		out.Egress = "none"
	}
	if out.Egress != "internet" {
		for _, d := range knet.Blocked(knet.Plan(mc.NetIndex, mc.ID)) {
			out.Total += d.Count
			out.Destinations = append(out.Destinations, api.BlockedDest{Dest: d.Dest, Kind: d.Kind, Count: d.Count})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
