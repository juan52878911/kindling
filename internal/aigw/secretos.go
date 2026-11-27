package aigw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// SEMILLA de la ronda 9 (analisis/ronda9-carga/plan-r9.md, pendiente 1 de la
// ronda 8). Dueño: Z4 (planificador, repuesto y secretos).
//
// Tres clases de secreto (docs y SECURITY.md): A, de entrada, la pone el
// gateway (Auth) y no se trata aquí; B, de salida, es una credencial de
// plantilla del proxy del daemon y nunca entra en el invitado
// (DeliverCredential); C, en claro, va al MMDS de cada réplica tras
// restaurarla (DeliverMMDS) y deja la réplica marcada: no se congela.

// Formas de entregar un secreto a un backend (SecretConfig.Deliver).
const (
	// DeliverCredential: secreto de SALIDA (backend → tercero). El gateway no
	// entrega nada a la réplica: ata una credencial de plantilla al dorado
	// (proxy de credenciales del daemon) y la réplica solo lleva un marcador.
	// Se rota cambiando File y recargando, sin dorado nuevo. Exige Domain y
	// Allow.
	DeliverCredential = "credential"
	// DeliverMMDS: secreto que el proceso necesita EN CLARO. El gateway lo
	// pone en el almacén MMDS de cada réplica tras restaurarla
	// (scheduler.AfterRestore) y corre los ganchos de la imagen. La réplica
	// queda marcada (no se congela ni se bifurca): exige idle remove o pause.
	DeliverMMDS = "mmds"
)

// SecretConfig es un secreto de un backend tal como se escribe en ai.json
// ("secrets" de un modelo kind http con snapshot). El valor nunca va en el
// fichero: está en File.
type SecretConfig struct {
	// Name identifica el secreto dentro del bloque: clave del almacén MMDS
	// ("secrets".<name>) o nombre de la credencial. [a-z0-9_-]+, único.
	Name string `json:"name"`
	// Deliver es una de las constantes Deliver*.
	Deliver string `json:"deliver"`
	// File es el fichero con el valor: relativo al ai.json, 0600, como
	// Auth.File. Se lee al usarlo; su hash es la "generación" con la que se
	// detecta una rotación en Reload.
	File string `json:"file"`
	// Env es la variable del servicio que lleva el marcador (solo
	// DeliverCredential).
	Env string `json:"env,omitempty"`
	// Domain es el nombre al que el backend dirige la petición y que el proxy
	// de credenciales intercepta (solo DeliverCredential).
	Domain string `json:"domain,omitempty"`
	// Upstream es el destino fijado por el operador, "host:puerto" (solo
	// DeliverCredential; necesita la credencial HTTP con upstream de Z6). Vacío
	// = el propio Domain, que tiene que ser público.
	Upstream string `json:"upstream,omitempty"`
	// Allow son las peticiones "MÉTODO /ruta" que el proxy deja pasar con la
	// credencial; obligatorio con DeliverCredential (el proxy es un oráculo de
	// la clave frente al invitado).
	Allow []string `json:"allow,omitempty"`
}

// Topes de "secrets": el fichero lo escribe una persona y lo lee un servicio.
const (
	maxSecrets      = 16
	maxSecretBytes  = 64 << 10
	maxSecretAllows = 32
	// plazoGanchos acota los ganchos tras restaurar de la imagen que aplican
	// los secretos en claro.
	plazoGanchos = 10 * time.Second
)

var (
	nombreSecreto = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	nombreEnv     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	nombreDominio = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`)
)

// idleEfectivo es la política de ociosidad que tendrá el bloque m una vez
// expandido (el defecto de expandBackend).
func idleEfectivo(m *ModelConfig) string {
	if m.Idle != "" {
		return m.Idle
	}
	if contratoDe(m) == ContractChispaClassify {
		return IdleFreeze
	}
	return IdleRemove
}

// validateSecrets valida el bloque "secrets" del modelo n: solo kind http con
// snapshot, Name único, Deliver conocido, File absoluto, regular y 0600 (como
// checkAuth), DeliverMMDS con idle remove o pause y sin campos de credencial,
// DeliverCredential con Env, Domain y Allow. Ningún error lleva el contenido
// del fichero: no se lee aquí.
func validateSecrets(n string, m *ModelConfig) []error {
	if len(m.Secrets) == 0 {
		return nil
	}
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("model %q: "+format, append([]any{n}, a...)...))
	}
	if m.Kind != KindHTTP || m.Snapshot == "" {
		add("secrets only apply to a kind http model with a snapshot (they are delivered to its replicas)")
		return errs
	}
	if len(m.Secrets) > maxSecrets {
		add("at most %d secrets", maxSecrets)
	}
	nombres, envs := map[string]bool{}, map[string]bool{}
	for i, s := range m.Secrets {
		donde := fmt.Sprintf("secrets[%d]", i)
		if !nombreSecreto.MatchString(s.Name) {
			add("%s.name must be [a-z0-9_-], up to 64 characters", donde)
		} else if nombres[s.Name] {
			add("%s.name %q is repeated", donde, s.Name)
		}
		nombres[s.Name] = true
		switch {
		case s.File == "":
			add("%s.file is required", donde)
		case !filepath.IsAbs(s.File):
			add("%s.file %q is relative: it must be an absolute path", donde, s.File)
		default:
			fi, err := os.Stat(s.File)
			switch {
			case err != nil:
				add("%s.file: %v", donde, err)
			case !fi.Mode().IsRegular():
				add("%s.file %s is not a regular file", donde, s.File)
			case fi.Mode().Perm()&0o077 != 0:
				add("%s.file %s must be 0600 (it is %04o)", donde, s.File, fi.Mode().Perm())
			case fi.Size() > maxSecretBytes:
				add("%s.file %s is larger than %d bytes", donde, s.File, maxSecretBytes)
			}
		}
		switch s.Deliver {
		case DeliverMMDS:
			if p := idleEfectivo(m); p != IdleRemove && p != IdlePause {
				add("%s: deliver mmds needs idle remove or pause (a replica holding a secret in memory is never frozen), not %q", donde, p)
			}
			if s.Env != "" || s.Domain != "" || s.Upstream != "" || len(s.Allow) > 0 {
				add("%s: env, domain, upstream and allow only apply to deliver credential", donde)
			}
		case DeliverCredential:
			if !nombreEnv.MatchString(s.Env) {
				add("%s.env must be an environment variable name ([A-Z_][A-Z0-9_]*)", donde)
			} else if envs[s.Env] {
				add("%s.env %q is repeated", donde, s.Env)
			}
			envs[s.Env] = true
			if len(s.Domain) > 253 || !nombreDominio.MatchString(s.Domain) {
				add("%s.domain must be a lowercase host name", donde)
			}
			if len(s.Allow) == 0 {
				add("%s.allow is required with deliver credential (the proxy would sign any request of the replica)", donde)
			}
			if len(s.Allow) > maxSecretAllows {
				add("%s.allow has more than %d entries", donde, maxSecretAllows)
			}
			for _, a := range s.Allow {
				if _, err := parseRoute(a); err != nil {
					add("%s.allow: %v", donde, err)
				}
			}
			if s.Upstream != "" {
				if h, p, err := net.SplitHostPort(s.Upstream); err != nil || h == "" || p == "" {
					add("%s.upstream must be host:port", donde)
				}
			}
		default:
			add("%s.deliver must be %q or %q, not %q", donde, DeliverCredential, DeliverMMDS, s.Deliver)
		}
	}
	return errs
}

// leerSecreto lee el valor de un secreto: fichero regular, 0600 y de como
// mucho maxSecretBytes, sin el salto de línea final. Se lee en cada uso,
// nunca se copia a ai.json ni a etiquetas. Los errores no llevan contenido.
func leerSecreto(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular 0600 file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSecretBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSecretBytes)
	}
	b = []byte(strings.TrimRight(string(b), "\r\n"))
	if len(b) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return b, nil
}

// generacion es la huella de un valor: lo que se compara en Reload para saber
// si un secreto rotó. No es el valor ni sirve para recuperarlo.
func generacion(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

// tapar quita de err los valores dados: un daemon o un invitado que repite lo
// que recibió no puede hacer que un secreto acabe en un log o en una
// respuesta.
func tapar(err error, valores ...[]byte) error {
	if err == nil {
		return nil
	}
	txt := err.Error()
	for _, v := range valores {
		if len(v) > 0 {
			txt = strings.ReplaceAll(txt, string(v), "[secret]")
		}
	}
	return errors.New(txt)
}

// secretosVivos es lo que el gateway sabe de los secretos ya entregados: la
// generación de cada uno por dorado, para detectar rotaciones en Reload.
type secretosVivos struct {
	mu  sync.Mutex
	gen map[string]map[string]string // dorado -> nombre -> generación
}

// aplicarSecretos lee los secretos de todos los backends con dorado y:
//   - para los de entrega credential, si alguno es nuevo o rotó, ata las
//     credenciales del bloque al dorado (las recibe cada réplica que nazca) y
//     a las réplicas vivas de ese dorado (SetCredentials las rota);
//   - para los mmds, si rotó alguno que ya se conocía, retira las réplicas
//     del dorado (scheduler.Retire): no reciben peticiones nuevas y se borran
//     al acabar las que tienen en vuelo; las nuevas nacen con el valor nuevo.
//
// Lo llaman Start y Reload. Un fichero que no se puede leer se avisa (sin
// contenido) y se deja como estaba.
func (g *Gateway) aplicarSecretos(ctx context.Context) {
	if g.opts.Client == nil {
		return
	}
	var vivas []*api.Machine
	listadas := false
	for _, b := range g.config().Backends() {
		if b == nil || b.Snapshot == "" || len(b.Secrets) == 0 {
			continue
		}
		g.secretos.mu.Lock()
		antes := g.secretos.gen[b.Snapshot]
		g.secretos.mu.Unlock()
		ahora := map[string]string{}
		var creds []api.CredentialSpec
		var valores [][]byte
		credCambio, mmdsCambio, fallo := false, false, false
		for _, s := range b.Secrets {
			v, err := leerSecreto(s.File)
			if err != nil {
				log.Printf("model %s: secret %s: %v (kept as it was)", b.Name, s.Name, err)
				fallo = true
				if old, ok := antes[s.Name]; ok {
					ahora[s.Name] = old
				}
				continue
			}
			gen := generacion(v)
			ahora[s.Name] = gen
			old, conocido := antes[s.Name]
			switch s.Deliver {
			case DeliverCredential:
				valores = append(valores, v)
				creds = append(creds, api.CredentialSpec{Domain: s.Domain, Env: s.Env, Secret: string(v),
					Allow: append([]string(nil), s.Allow...), Upstream: s.Upstream})
				if !conocido || old != gen {
					credCambio = true
				}
			case DeliverMMDS:
				if conocido && old != gen {
					mmdsCambio = true
				}
			}
		}
		if credCambio && !fallo {
			req := api.CredentialsRequest{Credentials: creds}
			if _, err := g.opts.Client.SetSnapshotCredentials(ctx, b.Snapshot, req); err != nil {
				log.Printf("model %s: binding credentials to %s: %v", b.Name, b.Snapshot, tapar(err, valores...))
				continue // sin guardar la generación: el siguiente Reload lo reintenta
			}
			if !listadas {
				listadas = true
				ms, err := g.opts.Client.List(ctx)
				if err != nil {
					log.Printf("listing machines to rotate credentials: %v", err)
				}
				vivas = ms
			}
			for _, m := range vivas {
				if m.Labels[LabelGateway] != g.opts.ID || doradoDe(m) != b.Snapshot {
					continue
				}
				if m.State != api.StateRunning && m.State != api.StatePaused {
					continue
				}
				if _, err := g.opts.Client.SetCredentials(ctx, m.ID, req); err != nil {
					log.Printf("model %s: rotating credentials of %s: %v", b.Name, m.Name, tapar(err, valores...))
				}
			}
		}
		if mmdsCambio {
			log.Printf("model %s: a secret delivered in memory changed; retiring its replicas", b.Name)
			g.retirar(ctx, b.Snapshot)
		}
		g.secretos.mu.Lock()
		if g.secretos.gen == nil {
			g.secretos.gen = map[string]map[string]string{}
		}
		g.secretos.gen[b.Snapshot] = ahora
		g.secretos.mu.Unlock()
	}
}

// entregarSecretos es el AfterRestore del gateway: pone los secretos mmds del
// backend de snap en el almacén MMDS de la réplica recién restaurada (clave
// "secrets") y corre los ganchos tras restaurar de la imagen, que los aplican.
// La máquina queda marcada HasSecrets (el daemon no la deja congelar). Sin
// secretos mmds no hace nada ni llama al daemon. Ningún error lleva un valor.
func (g *Gateway) entregarSecretos(ctx context.Context, snap string, mc *api.Machine) error {
	b := g.backendDe(snap)
	if b == nil || g.opts.Client == nil {
		return nil
	}
	doc := map[string]string{}
	var valores [][]byte
	for _, s := range b.Secrets {
		if s.Deliver != DeliverMMDS {
			continue
		}
		v, err := leerSecreto(s.File)
		if err != nil {
			return fmt.Errorf("secret %s: %w", s.Name, err)
		}
		doc[s.Name] = string(v)
		valores = append(valores, v)
	}
	if len(doc) == 0 {
		return nil
	}
	if _, err := g.opts.Client.PutMMDS(ctx, mc.ID, map[string]any{"secrets": doc}); err != nil {
		return fmt.Errorf("putting the replica secrets: %w", tapar(err, valores...))
	}
	mc.HasSecrets = true
	res, err := g.opts.Client.RunHooks(ctx, mc.ID, plazoGanchos)
	if err != nil {
		return fmt.Errorf("running the replica hooks: %w", tapar(err, valores...))
	}
	if !res.OK() {
		return tapar(fmt.Errorf("the replica hooks did not apply the secrets (%s): %s", res.Ready, res.Detail), valores...)
	}
	return nil
}
