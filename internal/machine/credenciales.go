package machine

// Credenciales del proxy de credenciales (pkg/credproxy): qué
// guarda el daemon de ellas y cómo las entrega y las vuelve a entregar.
//
// EL PROBLEMA: el proxy y el resolver son goroutines del daemon. Un reinicio
// del daemon se las lleva, y con ellas la clave, que solo vivía en su memoria:
// el dominio dejaba de resolverse (fallaba cerrado, pero fallaba) y había que
// volver a entregar la credencial a mano a cada máquina.
//
// LA SOLUCIÓN: cada máquina guarda sus credenciales cifradas en su directorio,
// machines/<id>/credentials.enc, con una clave que solo existe en este host y
// solo lee root. Es la misma clave maestra que firma los snapshots
// (secrets/snapshot.key), derivada con HKDF para que la clave de cifrar no sea
// la de firmar. AES-256-GCM con el id de la máquina como dato autenticado: un
// fichero copiado al directorio de otra máquina no descifra.
//
// POR QUÉ ASÍ Y NO EN state.json: state.json es la foto pública del daemon
// (`kling ps`, `kling inspect`, la API), se escribe con debounce fuera del
// candado y lo leen herramientas; una clave ahí es una clave en todas partes.
// El fichero cifrado no viaja: Commit copia snap.file, mem.file y overlay.ext4
// —nada más—, así que un dorado no lo lleva, y Remove borra el directorio
// entero. En state.json solo van los nombres de los dominios.
//
// CUÁNDO SE VUELVE A ENTREGAR: al reconciliar tras un reinicio del daemon (la
// máquina sigue viva, su MMDS también; solo hay que rehacer proxy y resolver) y
// al descongelar (la red pudo desmontarse mientras dormía, y el VMM es nuevo:
// se rehacen los dos y se reponen los marcadores en MMDS).
//
// EN macOS el proxy y el DNS no son del daemon sino de cada kling-vz, que los
// sirve en la pasarela de su pila de red (vz/internal/vnet). La clave viaja del
// daemon al ayudante por su socket de API y vive en la memoria de ese proceso,
// que corre como el mismo usuario que el daemon: es el cambio de modelo que
// cuenta SECURITY.md §7. Un reinicio del daemon no la pierde (kling-vz sigue
// vivo); un thaw sí (el kling-vz es nuevo), y por eso se reentrega igual.
//
// CREDENCIALES DE PLANTILLA: el caso MCP no entrega claves a una máquina, sino
// a un SERVICIO: el gateway instancia réplicas del dorado a demanda, las
// congela y las despierta, y nadie está delante para hacer `machine
// credential` a cada una. Por eso una plantilla puede llevar credenciales
// (secrets/credentials/<plantilla>.enc, sin marcadores: se generan por
// instancia) y runFrom las entrega a cada máquina que nace de ella antes de
// devolverla. El puente lee MMDS al lanzar cada sesión (ext/mcp, sessionEnv),
// así que el servidor MCP arranca ya con el marcador en su entorno. Viven
// aparte del directorio del snapshot a propósito: `kling mcp import` borra y
// rehace el snapshot al actualizar un servicio, y las claves deben sobrevivir
// a eso; se quitan explícitamente con Clear.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

const (
	credFile = "credentials.enc"
	// credInfo separa la clave de cifrado de la de firma aunque salgan de la
	// misma maestra. Cambiarla invalidaría todo almacén existente.
	credInfo = "kindling credential store v1"
)

// registrarCredenciales entrega el juego completo al proxy de la máquina:
// en Linux, knet.SetCredentials (proxy y resolver del daemon); en macOS, PUT
// /kling/credentials a su kling-vz, que sirve el proxy y el DNS dentro de su
// pila de red (plataforma_vz.go). En variable para que los tests del manager
// corran sin netns ni veth ni ayudante.
//
// resolve es el ResolveMachine del proxy de ESTA máquina (m.resolverCopia):
// solo lo usa el proxy de Linux, en cada conexión de una credencial con
// UpstreamMachine.
var registrarCredenciales = registrarCredencialesPlataforma

// reEnvCredencial es el nombre de la variable de entorno que recibe el marcador.
var reEnvCredencial = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// claveCredenciales deriva la clave de cifrado del almacén.
func (m *Manager) claveCredenciales() ([]byte, error) {
	master, err := m.claveFirma()
	if err != nil {
		return nil, fmt.Errorf("credential store key: %w", err)
	}
	return hkdf.Key(sha256.New, master, nil, credInfo, 32)
}

func (m *Manager) credPath(id string) string { return filepath.Join(m.dir(id), credFile) }

// credAuditPath es el registro de auditoría del proxy de la máquina (ver
// credaudit.go). En Linux lo escribe el proxy del daemon en <root>/audit, un
// directorio solo de root; en macOS, el kling-vz de la máquina, que lo deja
// junto a su socket, en el directorio de la máquina.
func (m *Manager) credAuditPath(id string) string {
	if auditoriaEnElDaemon {
		return filepath.Join(m.dirAuditoria(), id+".jsonl")
	}
	return filepath.Join(m.dir(id), credproxy.AuditFile)
}

// credSnapPath es el almacén de una plantilla. Bajo secrets/, no bajo el
// snapshot: sobrevive a que el snapshot se rehaga (ver la cabecera).
func (m *Manager) credSnapPath(name string) string {
	return filepath.Join(m.root, "secrets", "credentials", name+".enc")
}

// sellar cifra v con la clave del almacén; aad ata el resultado a su dueño.
func (m *Manager) sellar(v any, aad string) ([]byte, error) {
	key, err := m.claveCredenciales()
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	aead, err := aeadDe(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, []byte(aad)), nil
}

// abrir descifra lo que selló sellar con el mismo aad.
func (m *Manager) abrir(sellado []byte, aad string, v any) error {
	key, err := m.claveCredenciales()
	if err != nil {
		return err
	}
	aead, err := aeadDe(key)
	if err != nil {
		return err
	}
	if len(sellado) < aead.NonceSize() {
		return errors.New("credential store: truncated")
	}
	plain, err := aead.Open(nil, sellado[:aead.NonceSize()], sellado[aead.NonceSize():], []byte(aad))
	if err != nil {
		return fmt.Errorf("credential store of %s: can't decrypt it (another host's key, or the file was moved or tampered with)", aad)
	}
	return json.Unmarshal(plain, v)
}

// escribirSellado deja el fichero con 0600 y por rename: o está el almacén
// anterior entero o el nuevo entero. Con sellado vacío lo borra.
func escribirSellado(ruta string, sellado []byte) error {
	if sellado == nil {
		if err := os.Remove(ruta); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(ruta), 0o700); err != nil {
		return err
	}
	tmp := ruta + ".tmp"
	if err := os.WriteFile(tmp, sellado, 0o600); err != nil {
		return fmt.Errorf("writing the credential store: %w", err)
	}
	if err := os.Rename(tmp, ruta); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// guardarCredenciales cifra y escribe el juego completo de credenciales de la
// máquina id; con la lista vacía borra el fichero.
func (m *Manager) guardarCredenciales(id string, creds []credproxy.Credential) error {
	if len(creds) == 0 {
		return escribirSellado(m.credPath(id), nil)
	}
	sellado, err := m.sellar(creds, id)
	if err != nil {
		return err
	}
	return escribirSellado(m.credPath(id), sellado)
}

// cargarCredenciales lee y descifra el almacén de la máquina id. Sin fichero
// devuelve nil, nil: no tener credenciales no es un error.
func (m *Manager) cargarCredenciales(id string) ([]credproxy.Credential, error) {
	sellado, err := os.ReadFile(m.credPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var creds []credproxy.Credential
	if err := m.abrir(sellado, id, &creds); err != nil {
		return nil, err
	}
	for i := range creds {
		c := &creds[i]
		m.normalizarAlmacen("machine "+shortID(id), c.Kind, c.Database, c.Env, c.Domain, &c.AnyDatabase)
	}
	return creds, nil
}

// normalizarAlmacen es la ÚNICA puerta por la que una credencial recién
// descifrada (de máquina o de plantilla) pasa por credproxy.NormalizarAlmacen:
// una postgres de un almacén anterior a -database obligatoria se lee como
// AnyDatabase, que es lo que permitía entonces. Eso amplía en silencio lo que
// el operador cree que dio, así que se avisa en el log, una vez por dueño y
// variable (las plantillas se leen en cada listado).
func (m *Manager) normalizarAlmacen(dueño, kind, database, env, domain string, anyDatabase *bool) {
	if !credproxy.NormalizarAlmacen(kind, database, anyDatabase) {
		return
	}
	if _, visto := m.avisosAnyDB.LoadOrStore(dueño+"\x00"+env, true); visto {
		return
	}
	log.Printf("warning: %s: postgres credential %s (%s) loaded as any_database (pre-upgrade store); rotate with -database",
		dueño, env, domain)
}

// anyDatabaseDe son las variables de las credenciales postgres y mysql que
// entran en cualquier base, ordenadas: lo que `kling inspect` enseña de ellas.
func anyDatabaseDe(creds []credproxy.Credential) []string {
	var out []string
	for _, c := range creds {
		if (c.Kind == credproxy.KindPostgres || c.Kind == credproxy.KindMySQL) && c.AnyDatabase {
			out = append(out, c.Env)
		}
	}
	sort.Strings(out)
	return out
}

func aeadDe(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// validarSpecs comprueba lo que llega de la API: dominio exacto, nombre de
// variable válido, clave presente, sin repetir variable y Allow bien formado.
// Normaliza el dominio y las entradas de Allow.
func validarSpecs(specs []api.CredentialSpec) error {
	if len(specs) > credproxy.MaxCredentials {
		return fmt.Errorf("at most %d credentials", credproxy.MaxCredentials)
	}
	vistos := map[string]bool{}
	for i := range specs {
		s := &specs[i]
		d, err := credproxy.ValidarDominio(s.Domain)
		if err != nil {
			return err
		}
		s.Domain = d
		if !reEnvCredencial.MatchString(s.Env) {
			return fmt.Errorf("credential for %s: env %q must match [A-Z_][A-Z0-9_]*", d, s.Env)
		}
		if s.Secret == "" {
			return fmt.Errorf("credential for %s (%s): the key is empty", d, s.Env)
		}
		if vistos[s.Env] {
			return fmt.Errorf("env %s used by two credentials", s.Env)
		}
		vistos[s.Env] = true
		// Lo que depende del tipo (Allow, o puerto, rol, base, CA y forma de
		// la clave para Postgres) lo valida el proxy, que pone además el
		// puerto por defecto.
		c := credencialDeSpec(*s)
		if err := credproxy.ValidarTipo(&c); err != nil {
			return fmt.Errorf("%w (%s)", err, s.Env)
		}
		s.Port = c.Port
		s.Upstream, s.UpstreamTLS, s.TLSServerName = c.Upstream, c.UpstreamTLS, c.TLSServerName
	}
	return nil
}

// sinUpstreamMaquina rechaza specs con UpstreamMachine donde no tienen sentido
// (credenciales de plantilla: cada instancia quedaría atada a la misma copia
// sin que nadie lo pidiera para ella).
func sinUpstreamMaquina(specs []api.CredentialSpec) error {
	for _, s := range specs {
		if s.UpstreamMachine != "" || s.UpstreamOwner != "" {
			return fmt.Errorf("credential %s: upstream_machine is only for a machine's own credentials (kling db attach), not for templates", s.Env)
		}
	}
	return nil
}

// credencialDeSpec es la credencial del proxy que describe s, sin marcador.
func credencialDeSpec(s api.CredentialSpec) credproxy.Credential {
	return credproxy.Credential{Env: s.Env, Domain: s.Domain, Secret: s.Secret, Allow: s.Allow,
		Kind: s.Type, Port: s.Port, User: s.User, Database: s.Database, AnyDatabase: s.AnyDatabase, CAPEM: s.CAPEM,
		Upstream: s.Upstream, UpstreamTLS: s.UpstreamTLS, TLSServerName: s.TLSServerName,
		UpstreamMachine: s.UpstreamMachine, UpstreamOwner: s.UpstreamOwner}
}

// fusionarSpecs aplica specs sobre previas por Env: la misma variable se
// sustituye entera, Allow incluido (rotación), las demás se añaden.
func fusionarSpecs(previas, specs []api.CredentialSpec) []api.CredentialSpec {
	out := append([]api.CredentialSpec(nil), previas...)
	for _, s := range specs {
		hecho := false
		for i := range out {
			if out[i].Env == s.Env {
				out[i] = s
				hecho = true
				break
			}
		}
		if !hecho {
			out = append(out, s)
		}
	}
	return out
}

// entregarCredenciales es la entrega en sí a una máquina viva con egress
// allowlist: fusiona specs con lo que ya tenía (por variable, conservando el
// marcador para que una rotación no rompa el entorno del proceso), guarda el
// almacén, pone los marcadores en MMDS y registra el juego completo en el
// proxy. En ese orden: si falla el almacén no ha cambiado nada; si falla MMDS,
// el almacén tiene una credencial que el invitado aún no ve y la siguiente
// entrega (o un thaw) la repone; y un marcador sin proxy detrás no vale nada
// fuera de esta máquina. Devuelve el juego completo y cuántas eran nuevas.
func (m *Manager) entregarCredenciales(ctx context.Context, id string, netcfg *knet.Net, c *fc.Client,
	specs []api.CredentialSpec) (creds []credproxy.Credential, nuevas int, err error) {
	if err := validarSpecs(specs); err != nil {
		return nil, 0, err
	}
	previas, err := m.cargarCredenciales(id)
	if err != nil {
		return nil, 0, err
	}
	creds = append([]credproxy.Credential(nil), previas...)
	porEnv := make(map[string]int, len(creds))
	for i, cr := range creds {
		porEnv[cr.Env] = i
	}
	for _, s := range specs {
		// Allow va con la clave: rotar sin él la deja sin restricciones, como
		// una credencial nueva. Es lo que se pidió, y así la API no tiene un
		// "conservar lo de antes" implícito que nadie ve.
		// Igual el tipo y sus campos: se sustituyen enteros con la clave.
		if i, ok := porEnv[s.Env]; ok {
			ph := creds[i].Placeholder
			creds[i] = credencialDeSpec(s)
			creds[i].Placeholder = ph
			continue
		}
		ph, err := credproxy.NuevoMarcador()
		if err != nil {
			return nil, 0, err
		}
		c := credencialDeSpec(s)
		c.Placeholder = ph
		creds = append(creds, c)
		porEnv[s.Env] = len(creds) - 1
		nuevas++
	}
	if err := credproxy.ValidarCredenciales(creds); err != nil {
		return nil, 0, err
	}
	if err := m.comprobarUpstreams(creds); err != nil {
		return nil, 0, err
	}
	// Las que van a otra máquina (kling db attach): se comprueba ya lo que el
	// proxy volverá a comprobar en cada conexión, para que un attach
	// imposible falle aquí y no en el primer psql del agente.
	if err := m.comprobarCopias(id, specs); err != nil {
		return nil, 0, err
	}
	if err := m.guardarCredenciales(id, creds); err != nil {
		return nil, 0, err
	}
	if err := ponerMarcadoresMMDS(ctx, c, creds); err != nil {
		return nil, 0, err
	}
	if err := registrarCredenciales(ctx, c, netcfg, creds, m.credAuditPath(id), m.resolverCopia(id)); err != nil {
		return nil, 0, err
	}
	return creds, nuevas, nil
}

// ponerMarcadoresMMDS deja en MMDS (clave "env") el marcador de cada
// credencial. PATCH fusiona con lo que otro inyectara antes; Firecracker lo
// rechaza si el almacén aún no existe, y entonces se crea con PUT.
func ponerMarcadoresMMDS(ctx context.Context, c *fc.Client, creds []credproxy.Credential) error {
	env := make(map[string]string, len(creds))
	for _, cr := range creds {
		env[cr.Env] = cr.Placeholder
	}
	doc := map[string]any{"env": env}
	err := c.PatchMMDSData(ctx, doc)
	if err != nil && strings.Contains(err.Error(), "not initialized") {
		err = c.PutMMDSData(ctx, doc)
	}
	if err != nil {
		return fmt.Errorf("injecting the placeholders via MMDS: %w", err)
	}
	return nil
}

// conMarcadores devuelve data con los marcadores de las credenciales de la
// máquina id en su "env", para un PUT /mmds que sustituye el almacén entero
// (PutMMDS). Sin credenciales, data tal cual. Un marcador pisa a una variable
// del mismo nombre que traiga data: esa variable es de la credencial, y darle
// otro valor la rompería sin que nadie lo note.
func (m *Manager) conMarcadores(id string, data any) (any, error) {
	creds, err := m.cargarCredenciales(id)
	if err != nil {
		return nil, fmt.Errorf("reading the credentials to keep their placeholders: %w", err)
	}
	if len(creds) == 0 {
		return data, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("the MMDS store must be a JSON object on a machine with credentials: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	env, _ := doc["env"].(map[string]any)
	if doc["env"] != nil && env == nil {
		return nil, errors.New(`"env" in the MMDS store must be an object on a machine with credentials`)
	}
	if env == nil {
		env = map[string]any{}
	}
	for _, cr := range creds {
		env[cr.Env] = cr.Placeholder
	}
	doc["env"] = env
	return doc, nil
}

// reentregarCredenciales vuelve a registrar en el proxy y el resolver las
// credenciales guardadas de mc, cuya red acaba de rehacerse (o cuyo daemon
// acaba de arrancar). Con c distinto de nil repone además los marcadores en
// MMDS: tras un thaw el VMM es nuevo. Con c nil (reinicio del daemon con la
// máquina viva) en macOS no hay nada que rehacer: el proxy vive en kling-vz,
// que sobrevive al daemon con sus claves. Devuelve cuántas credenciales
// entregó.
//
// Anota además en mc CredentialAnyDatabase (lo que dice `kling inspect`): una
// máquina de antes de ese campo lo recupera al primer reconcile o thaw. mc es
// la viva en reconcile (con m.mu tomado) y una copia en Thaw, que la pasa a la
// viva al final.
func (m *Manager) reentregarCredenciales(ctx context.Context, mc *api.Machine, c *fc.Client) (int, error) {
	creds, err := m.cargarCredenciales(mc.ID)
	if err != nil || len(creds) == 0 {
		return 0, err
	}
	mc.CredentialAnyDatabase = anyDatabaseDe(creds)
	if c != nil {
		// Un reenvío pudo abrirse en ese puerto desde la última entrega. Con
		// c nil (reconcile, m.mu tomado) no se mira: en Linux no hay
		// reenvíos, y en macOS no se registra nada nuevo.
		if err := m.comprobarUpstreams(creds); err != nil {
			return 0, err
		}
		if err := ponerMarcadoresMMDS(ctx, c, creds); err != nil {
			return 0, err
		}
	}
	if err := registrarCredenciales(ctx, c, knet.Plan(mc.NetIndex, mc.ID), creds, m.credAuditPath(mc.ID), m.resolverCopia(mc.ID)); err != nil {
		return 0, err
	}
	return len(creds), nil
}

// dominiosDe son los dominios de un juego de credenciales, ordenados y sin
// repetir: lo único de ellas que va a state.json.
func dominiosDe(creds []credproxy.Credential) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range creds {
		if !seen[c.Domain] {
			seen[c.Domain] = true
			out = append(out, c.Domain)
		}
	}
	sort.Strings(out)
	return out
}

// ── credenciales de plantilla ────────────────────────────────────────────────

// cargarCredencialesPlantilla lee las credenciales atadas a la plantilla name.
// Sin fichero, nil, nil.
func (m *Manager) cargarCredencialesPlantilla(name string) ([]api.CredentialSpec, error) {
	sellado, err := os.ReadFile(m.credSnapPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []api.CredentialSpec
	if err := m.abrir(sellado, "snapshot:"+name, &specs); err != nil {
		return nil, err
	}
	// Igual que en cargarCredenciales.
	for i := range specs {
		s := &specs[i]
		m.normalizarAlmacen("template "+name, s.Type, s.Database, s.Env, s.Domain, &s.AnyDatabase)
	}
	return specs, nil
}

// SetSnapshotCredentials ata credenciales a una plantilla (o, con clear, se las
// quita todas). Exige que la plantilla exista y tenga egress allowlist, que es
// lo que cada instancia necesitará para que se le puedan entregar.
func (m *Manager) SetSnapshotCredentials(name string, specs []api.CredentialSpec, clear bool) (*api.Snapshot, error) {
	snap, err := m.loadSnapshot(name)
	if err != nil {
		return nil, err
	}
	if clear {
		if err := escribirSellado(m.credSnapPath(name), nil); err != nil {
			return nil, err
		}
		return m.Snapshot(name)
	}
	if len(specs) == 0 {
		return nil, errors.New("no credentials given")
	}
	if snap.Egress != string(knet.EgressAllowlist) {
		return nil, fmt.Errorf("template %s has egress %q; credentials need a template imported with -egress allowlist",
			name, snap.Egress)
	}
	if err := validarSpecs(specs); err != nil {
		return nil, err
	}
	if err := sinUpstreamMaquina(specs); err != nil {
		return nil, err
	}
	if err := m.comprobarUpstreams(credencialesDeSpecs(specs)); err != nil {
		return nil, err
	}
	previas, err := m.cargarCredencialesPlantilla(name)
	if err != nil {
		return nil, err
	}
	todas := fusionarSpecs(previas, specs)
	if len(todas) > credproxy.MaxCredentials {
		return nil, fmt.Errorf("at most %d credentials per template", credproxy.MaxCredentials)
	}
	// Postgres y MySQL juntos: MySQL en el 3306 y ninguna Postgres ahí (lo
	// comprobaría el proxy al arrancar cada instancia; mejor decirlo ya).
	if err := credproxy.ValidarPuertosDB(credencialesDeSpecs(todas)); err != nil {
		return nil, err
	}
	sellado, err := m.sellar(todas, "snapshot:"+name)
	if err != nil {
		return nil, err
	}
	if err := escribirSellado(m.credSnapPath(name), sellado); err != nil {
		return nil, err
	}
	return m.Snapshot(name)
}

// anotarCredencialesPlantilla rellena s.CredentialDomains desde el almacén de
// la plantilla. Se lee al vuelo y no se guarda en meta.json: el almacén es la
// única fuente, y así no hay dos sitios que puedan discrepar.
func (m *Manager) anotarCredencialesPlantilla(s *api.Snapshot) {
	specs, err := m.cargarCredencialesPlantilla(s.Name)
	if err != nil || len(specs) == 0 {
		return
	}
	creds := credencialesDeSpecs(specs)
	s.CredentialDomains = dominiosDe(creds)
	s.CredentialAnyDatabase = anyDatabaseDe(creds)
}
