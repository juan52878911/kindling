package guest

// POST /drop-caches: soltar la caché de páginas del invitado.
//
// Lo que el kernel leyó de los discos (la raíz, los volúmenes) se queda en la
// caché de páginas, y para el host es memoria ocupada: ni el globo ni el free
// page reporting la reclaman, porque para el invitado no está libre. Tras un
// `apt install` o una compilación son decenas o cientos de MiB por VM, y un
// dorado guardado así los copia a cada instancia. El host la pide antes de
// apretar, congelar o guardar un dorado.
//
// Solo la caché de páginas (1), no dentries ni inodos (3): rehacerlos cuesta
// más y ocupan poco. Antes un sync, para que lo sucio también se pueda soltar.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// dropCachesPath es dónde se escribe; variable para los tests.
var dropCachesPath = "/proc/sys/vm/drop_caches"

// DropCachesHandler sirve POST /drop-caches.
func DropCachesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var res api.GuestDropCaches
		res.CachedBeforeMiB = cachedMiB()
		syscall.Sync()
		// O_WRONLY sin O_CREATE: fuera de Linux (o sin /proc) es un error, no
		// un fichero nuevo.
		f, err := os.OpenFile(dropCachesPath, os.O_WRONLY|os.O_TRUNC, 0)
		if err == nil {
			_, err = f.WriteString("1")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			http.Error(w, "drop caches: "+err.Error(), http.StatusInternalServerError)
			return
		}
		res.CachedAfterMiB = cachedMiB()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

// cachedMiB es el Cached de /proc/meminfo, en MiB (0 si no se puede leer).
func cachedMiB() int {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	return parseCached(f)
}

func parseCached(r io.Reader) int {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		if len(campos) >= 2 && campos[0] == "Cached:" {
			kb, _ := strconv.Atoi(campos[1])
			return kb / 1024
		}
	}
	return 0
}
