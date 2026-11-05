package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// huellaKernelInfo cachea el sha256 del vmlinux para GET /info por tamaño y
// fecha del fichero, como hace el manager para Commit: `kling doctor` y
// `kling status` preguntan /info a menudo, y rehashear decenas de MiB en cada
// llamada sería tirar el tiempo.
type huellaKernelInfo struct {
	mu    sync.Mutex
	tam   int64
	fecha int64
	hash  string
}

var kernelInfo huellaKernelInfo

// sha devuelve el sha256 de path, o "" si no existe o no se puede leer: /info
// no falla por esto, solo omite el campo.
func (h *huellaKernelInfo) sha(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	tam, fecha := st.Size(), st.ModTime().UnixNano()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hash != "" && h.tam == tam && h.fecha == fecha {
		return h.hash
	}
	hash, err := digest.File(path)
	if err != nil {
		return ""
	}
	h.tam, h.fecha, h.hash = tam, fecha, hash
	return hash
}

// kernelSelloCongelada es el kernel_sha256 del sello volcado.ok de una
// máquina congelada (formato estable, ver docs/compatibilidad.md), o "" si no
// lo lleva: los sellos anteriores a K2 no tienen con qué comparar.
func kernelSelloCongelada(root, id string) string {
	raw, err := os.ReadFile(filepath.Join(root, "machines", id, "volcado.ok"))
	if err != nil {
		return ""
	}
	var s struct {
		KernelSHA256 string `json:"kernel_sha256"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.KernelSHA256
}

// kernelesDistintos lista los dorados y las congeladas cuyo kernel grabado no
// es actual. Lo que no consta (sha vacío) no se cuenta: no hay con qué
// comparar, y avisar de todo dorado antiguo sería ruido. Sin kernel instalado
// tampoco: nada que comparar.
func kernelesDistintos(actual string, snaps []*api.Snapshot, congeladas map[string]string) []api.KernelMismatch {
	if actual == "" {
		return nil
	}
	var out []api.KernelMismatch
	for _, s := range snaps {
		if s != nil && s.KernelSHA256 != "" && s.KernelSHA256 != actual {
			out = append(out, api.KernelMismatch{Kind: "snapshot", Name: s.Name, KernelSHA256: s.KernelSHA256})
		}
	}
	nombres := make([]string, 0, len(congeladas))
	for n := range congeladas {
		nombres = append(nombres, n)
	}
	sort.Strings(nombres)
	for _, n := range nombres {
		if sha := congeladas[n]; sha != "" && sha != actual {
			out = append(out, api.KernelMismatch{Kind: "machine", Name: n, KernelSHA256: sha})
		}
	}
	return out
}

// infoKernel rellena en info el kernel instalado y, si toca (vista del host),
// lo grabado con otro.
func (s *Server) infoKernel(info *api.Info, vistaHost bool) {
	info.KernelSHA256 = kernelInfo.sha(s.mgr.KernelPath())
	if !vistaHost || info.KernelSHA256 == "" {
		return
	}
	congeladas := map[string]string{}
	for _, m := range s.mgr.List() {
		if m.State == api.StateWarm {
			congeladas[m.Name] = kernelSelloCongelada(s.root, m.ID)
		}
	}
	info.KernelMismatch = kernelesDistintos(info.KernelSHA256, s.mgr.Snapshots(), congeladas)
}
