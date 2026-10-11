package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// plazoProcesosVivos acota la pregunta de save al agente: es un aviso, no
// puede retrasar el volcado.
const plazoProcesosVivos = 2 * time.Second

// avisoProcesosVivos son los avisos de `kling save` cuando el dorado se lleva
// procesos de usuario vivos: todas sus copias los despiertan con los mismos
// aleatorios en memoria, porque /resync vuelve a sembrar el kernel y no el
// estado de cada proceso (#175), y cuando trae claves de host SSH, que todas
// comparten (#176). Vacío si no hay nada que decir: sin procesos
// (o todos del servicio con OnRestore), un agente que no sabe contarlos, o el
// puente MCP, cuyo servidor siempre está vivo en el dorado y no tiene
// OnRestore que ofrecer todavía.
func (m *Manager) avisoProcesosVivos(ctx context.Context, ref string) []string {
	mc, ok := m.Get(ref)
	if !ok || !mc.Reachable() || mc.State != api.StateRunning || !mc.Agent.Has(api.GuestCapProcs) || mc.Agent.Has(api.GuestCapMCP) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, plazoProcesosVivos)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+mc.Addr(api.GuestPort)+api.GuestProcsPath, nil)
	if err != nil {
		return nil
	}
	resp, err := listoClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var p api.GuestProcs
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&p) != nil {
		return nil
	}
	return slices.DeleteFunc([]string{textoProcesosVivos(p), textoClavesSSH(p.HostKeys)},
		func(s string) bool { return s == "" })
}

// textoClavesSSH es el aviso de las claves de host SSH del dorado: todas sus
// copias se presentan con la misma, y un cliente no distingue una de otra
// (#176). El agente no las renueva solo: sshd las carga al arrancar.
func textoClavesSSH(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf("the SSH host keys (%s) will be the same in every copy; regenerate them in an "+
		"/etc/kindling/post-restore.d hook (rm the keys, ssh-keygen -A, restart sshd), which runs in each copy",
		strings.Join(keys, ", "))
}

func textoProcesosVivos(p api.GuestProcs) string {
	n := p.Count - p.Covered
	if n <= 0 {
		return ""
	}
	que := "process is"
	if n != 1 {
		que = "processes are"
	}
	nombres := ""
	if len(p.Names) > 0 {
		nombres = " (" + strings.Join(p.Names, ", ") + ")"
	}
	return fmt.Sprintf("%d %s alive in this snapshot%s: every copy resumes them with the same in-memory random state "+
		"(Math.random, crypto.randomUUID, Python random, OpenSSL inside the process). Import the image with "+
		"-on-restore restart (or signal:SIGUSR1 if the app reseeds on a signal), or have the app watch %s; see docs/imagenes.md",
		n, que, nombres, api.GuestGenerationFile)
}
