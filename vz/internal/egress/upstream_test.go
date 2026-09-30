package egress

import (
	"net/netip"
	"testing"
)

// El DNS del invitado va a un resolver público y nunca al del Mac, que suele
// ser privado (router, VPN) y contesta la intranet por split-horizon. En un Mac
// cuyo /etc/resolv.conf apunta al router (192.168.x.1, lo normal en casa),
// antes NewResolver reenviaba ahí.
func TestUpstreamPublico(t *testing.T) {
	r := NewResolver(NewPolicy())
	ap, err := netip.ParseAddrPort(r.Upstream)
	if err != nil {
		t.Fatalf("upstream %q: %v", r.Upstream, err)
	}
	if IsBlockedIP(ap.Addr()) || IsForbiddenDest(ap.Addr()) {
		t.Fatalf("el DNS del invitado va a %s, una dirección privada o del Mac", r.Upstream)
	}
	if r.Upstream != DefaultUpstream {
		t.Fatalf("upstream %s, want %s", r.Upstream, DefaultUpstream)
	}
}
