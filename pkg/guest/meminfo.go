package guest

// GET /meminfo: la memoria del invitado vista desde dentro. En macOS el globo
// de Virtualization.framework no da estadísticas, y sin esto `squeeze`
// apretaba a ciegas (ver internal/machine: objetivoSinEstadisticas).

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// meminfoPath es de dónde se lee; variable para los tests.
var meminfoPath = "/proc/meminfo"

// parseMeminfo saca MemTotal y MemAvailable (en MiB) de /proc/meminfo.
func parseMeminfo(r io.Reader) (api.GuestMemInfo, bool) {
	var out api.GuestMemInfo
	var total, avail bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		if len(campos) < 2 {
			continue
		}
		kb, err := strconv.Atoi(campos[1])
		if err != nil {
			continue
		}
		switch campos[0] {
		case "MemTotal:":
			out.TotalMiB, total = kb/1024, true
		case "MemAvailable:":
			out.AvailableMiB, avail = kb/1024, true
		}
	}
	return out, total && avail
}

// MemInfoHandler sirve GET /meminfo.
func MemInfoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		f, err := os.Open(meminfoPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		mi, ok := parseMeminfo(f)
		if !ok {
			http.Error(w, "MemTotal or MemAvailable missing from "+meminfoPath, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mi)
	}
}
