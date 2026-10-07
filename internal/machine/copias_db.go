package machine

// El modelo A de `kling db` visto desde el manager: una credencial Postgres
// con UpstreamMachine lleva al agente, por su proxy, a una copia de base de
// datos que vive en OTRA máquina (docs/db.md, pkg/credproxy/maquina.go).
//
// El manager pone tres cosas:
//
//   - resolverCopia: el Options.ResolveMachine del proxy de cada agente. En
//     CADA conexión, bajo m.mu, comprueba que la copia con ese ID exacto
//     existe, corre, es una copia de kling db lista, que la copia, el agente y
//     la credencial dicen el mismo kling.db.owner y que la copia expone el
//     puerto (kling.ports); y solo entonces da su dirección. Nada de eso se
//     fija al entregar: el índice de red de una copia borrada pasa a otra.
//   - comprobarCopias: lo mismo al entregar la credencial (kling db attach),
//     para que un attach imposible falle ya y no en el primer psql.
//   - invalidar: al congelar, pausar, parar, borrar o marcar fallida una
//     máquina, o al cambiar sus etiquetas de kling db, se cortan las sesiones
//     vivas hacia ella en todos los proxies (y, si la que cambia es un agente,
//     las suyas).
//
// En Linux el proxy es del daemon y marca a la copia por la IP de su netns.
// En macOS el proxy es del kling-vz del agente, que pide cada conexión al
// broker del daemon (broker.go): la misma puerta, comprobarCopiaLocked, en
// cada conexión, y el daemon marca al reenvío de la copia.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Sustituibles en los tests: cortar sesiones es cosa de internal/net.
var (
	invalidarCopia   = invalidarCopiaPlataforma
	invalidarAgente  = invalidarAgentePlataforma
	invalidarEnlaces = invalidarEnlacesPlataforma
	invalidarOrigen  = invalidarOrigenPlataforma
)

// resolverCopia es el ResolveMachine del proxy de la máquina agente.
func (m *Manager) resolverCopia(agente string) credproxy.ResolveMachineFunc {
	return func(id, owner string, port int) (string, error) {
		// Una arista credential de un grafo: id es el de su nodo destino, no
		// el de una máquina (grafo_red.go).
		if gid, desde, hacia, ok := m.aristaVirtual(agente, id, owner); ok {
			ctx, cancel := context.WithTimeout(context.Background(), plazoDespertar)
			defer cancel()
			addr, _, err := m.resolverArista(ctx, agente, gid, desde, hacia, port, api.GraphEdgeCredential)
			return addr, err
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		cp, err := m.comprobarCopiaLocked(agente, id, owner, port)
		if err != nil {
			return "", err
		}
		return direccionCopiaLocked(cp, port)
	}
}

// comprobarCopiaLocked es la puerta del modelo A. Con m.mu tomado; devuelve la
// entrada viva de la copia.
func (m *Manager) comprobarCopiaLocked(agente, id, owner string, port int) (*api.Machine, error) {
	if err := credproxy.ValidarIDMaquina(id); err != nil {
		return nil, err
	}
	if agente == id {
		return nil, errors.New("a machine can't be its own upstream machine")
	}
	ag := m.byID[agente]
	if ag == nil {
		return nil, fmt.Errorf("agent machine %s no longer exists", shortID(agente))
	}
	cp := m.byID[id] // el ID exacto: ni nombres ni prefijos
	if cp == nil {
		return nil, fmt.Errorf("machine %s doesn't exist (removed? a new copy has a new id: attach it again)", shortID(id))
	}
	if cp.State != api.StateRunning {
		return nil, fmt.Errorf("machine %s is %s, not running", cp.Name, cp.State)
	}
	if cp.Labels[api.LabelDBGolden] == "" {
		return nil, fmt.Errorf("machine %s is not a kling db copy", cp.Name)
	}
	if st := cp.Labels[api.LabelDBState]; st != api.DBStateReady {
		return nil, fmt.Errorf("copy %s is not ready (%s=%q)", cp.Name, api.LabelDBState, st)
	}
	co, ao := cp.Labels[api.LabelDBOwner], ag.Labels[api.LabelDBOwner]
	if owner == "" || co != owner || ao != owner {
		return nil, fmt.Errorf("owner mismatch: copy %s is %q, agent %s is %q, the credential says %q",
			cp.Name, co, ag.Name, ao, owner)
	}
	// Y el mismo inquilino (kling.owner, docs/authz.md): kling.db.owner es
	// una etiqueta que elige quien crea la máquina; kling.owner la pone el
	// daemon con política. Sin política las dos están vacías y esto no cambia
	// nada.
	if cp.Labels[api.LabelOwner] != ag.Labels[api.LabelOwner] {
		return nil, fmt.Errorf("tenant mismatch: copy %s belongs to %q, agent %s to %q",
			cp.Name, cp.Labels[api.LabelOwner], ag.Name, ag.Labels[api.LabelOwner])
	}
	if !puertoExpuesto(cp.Labels[api.LabelPorts], port) {
		return nil, fmt.Errorf("copy %s does not expose port %d (%s)", cp.Name, port, api.LabelPorts)
	}
	return cp, nil
}

// puertoExpuesto dice si port está en la lista de kling.ports.
func puertoExpuesto(lista string, port int) bool {
	for _, p := range strings.Split(lista, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n == port {
			return true
		}
	}
	return false
}

// comprobarCopias hace, al entregar, la comprobación de cada conexión para las
// specs con UpstreamMachine. Sin m.mu tomado.
func (m *Manager) comprobarCopias(agente string, specs []api.CredentialSpec) error {
	for _, s := range specs {
		if s.UpstreamMachine == "" {
			continue
		}
		port := s.Port
		if port == 0 {
			port = credproxy.PGDefaultPort
		}
		if gid, desde, hacia, ok := m.aristaVirtual(agente, s.UpstreamMachine, s.UpstreamOwner); ok {
			// De un grafo: el destino puede ser un lazy sin instancia, así que
			// al entregar solo se comprueba la arista, no que corra.
			m.mu.RLock()
			_, _, err := m.comprobarAristaLocked(agente, gid, desde, hacia, port, api.GraphEdgeCredential)
			m.mu.RUnlock()
			if err != nil {
				return fmt.Errorf("credential %s: %w", s.Env, err)
			}
			continue
		}
		m.mu.RLock()
		cp, err := m.comprobarCopiaLocked(agente, s.UpstreamMachine, s.UpstreamOwner, port)
		if err == nil {
			_, err = direccionCopiaLocked(cp, port)
		}
		m.mu.RUnlock()
		if err != nil {
			return fmt.Errorf("credential %s: %w", s.Env, err)
		}
	}
	return nil
}

// invalidarSesiones corta las sesiones vivas hacia la máquina id en todos los
// proxies. Se llama SIN m.mu y después de cambiar el estado: una conexión que
// se resuelva después ya ve el estado nuevo, y una que se resolvió antes está
// registrada y se corta aquí.
func (m *Manager) invalidarSesiones(id, motivo string) {
	// Si es la máquina de un nodo de grafo, también lo que va a su nodo: las
	// credenciales de las aristas llevan el ID del nodo, y los enlaces que
	// aún resuelven hacia él (grafo_red.go).
	ids := []string{id}
	if v, ok := m.virtuales.Load(id); ok {
		ids = append(ids, v.(string))
	}
	n := 0
	for _, x := range ids {
		n += invalidarCopia(x)
	}
	if n > 0 {
		m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: id,
			Message: fmt.Sprintf("%d database session(s) from other machines cut (%s)", n, motivo)})
	}
	if n := invalidarEnlaces(ids...); n > 0 {
		m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: id,
			Message: fmt.Sprintf("%d graph link session(s) cut (%s)", n, motivo)})
	}
}

// etiquetasDB son las etiquetas de las que depende el modelo A.
var etiquetasDB = []string{api.LabelDBGolden, api.LabelDBOwner, api.LabelDBState, api.LabelPorts, api.LabelOwner}

// cambianEtiquetasDB dice si aplicar nuevas sobre antes cambia alguna de
// etiquetasDB.
func cambianEtiquetasDB(antes, nuevas map[string]string) bool {
	for _, k := range etiquetasDB {
		if v, ok := nuevas[k]; ok && v != antes[k] {
			return true
		}
	}
	return false
}

// RemoveCredential quita la credencial de variable env de una máquina (kling
// db detach, kling machine credential -rm). Con upstreamMachine no vacío,
// exige que esa credencial vaya a esa máquina: quien quita el attach de una
// copia no se lleva por error otra credencial con la misma variable.
//
// En una máquina viva se entrega al proxy el juego sin ella (que corta sus
// sesiones: ver pkg/credproxy.SetCredentials) y se borra su marcador de MMDS.
// En una congelada o parada solo cambia el almacén: el thaw entrega lo que
// haya en él.
func (m *Manager) RemoveCredential(ctx context.Context, ref, env, upstreamMachine string) (*api.Machine, error) {
	if !reEnvCredencial.MatchString(env) {
		return nil, fmt.Errorf("env %q must match [A-Z_][A-Z0-9_]*", env)
	}
	mc, ok := m.Get(ref)
	if !ok {
		return nil, noExiste(ref)
	}
	defer m.lock(mc.ID)()
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, noExiste(ref)
	}
	creds, err := m.cargarCredenciales(cur.ID)
	if err != nil {
		return nil, err
	}
	i := -1
	for j, c := range creds {
		if c.Env == env {
			i = j
			break
		}
	}
	if i < 0 {
		return nil, fmt.Errorf("machine %s has no credential in %s", cur.Name, env)
	}
	if upstreamMachine != "" && creds[i].UpstreamMachine != upstreamMachine {
		return nil, fmt.Errorf("the credential in %s of %s does not go to machine %s", env, cur.Name, shortID(upstreamMachine))
	}
	quitada := creds[i]
	// Una máquina corriendo sin socket del VMM no podría enterarse (en macOS,
	// kling-vz seguiría con la credencial): mejor no tocar nada que dejar el
	// almacén y el proxy en desacuerdo.
	if cur.State == api.StateRunning {
		m.mu.RLock()
		sinSock := m.socket[cur.ID] == ""
		m.mu.RUnlock()
		if sinSock {
			return nil, fmt.Errorf("machine %s is running but its VMM socket is not available yet: try again", cur.Name)
		}
	}
	creds = append(creds[:i:i], creds[i+1:]...)
	if err := m.guardarCredenciales(cur.ID, creds); err != nil {
		return nil, err
	}
	if cur.State == api.StateRunning {
		m.mu.RLock()
		sock := m.socket[cur.ID]
		m.mu.RUnlock()
		var c *fc.Client
		if sock != "" {
			c = fc.New(sock)
			// El marcador fuera de MMDS (null lo borra en un merge patch). No
			// es un secreto: esto es limpieza, no seguridad, y no se aborta.
			if err := c.PatchMMDSData(ctx, map[string]any{"env": map[string]any{env: nil}}); err != nil {
				m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStarted, ID: cur.ID, Name: cur.Name,
					Message: "warning: could not remove the placeholder of " + env + " from MMDS: " + err.Error()})
			}
		}
		if err := registrarCredenciales(ctx, c, knet.Plan(cur.NetIndex, cur.ID), creds, m.credAuditPath(cur.ID), m.resolverCopia(cur.ID)); err != nil {
			return nil, fmt.Errorf("the credential is gone from the store, but the proxy still has it until the next thaw: %w", err)
		}
	}

	m.mu.Lock()
	live := m.byID[cur.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q no longer exists", ref)
	}
	live.CredentialDomains = dominiosDe(creds)
	live.CredentialAnyDatabase = anyDatabaseDe(creds)
	m.persist()
	out := live.Clone()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStarted, ID: cur.ID, Name: cur.Name,
		Message: fmt.Sprintf("credential %s for %s withdrawn from the credential proxy", env, quitada.Domain)})
	return out, nil
}
