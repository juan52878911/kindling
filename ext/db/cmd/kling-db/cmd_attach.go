package main

// kling db attach / detach: el MODELO A. Una copia compartida por agentes que
// viven en OTRAS microVMs, a través del proxy de credenciales de Postgres de
// cada agente (pkg/credproxy): el agente recibe un marcador en PGPASSWORD y el
// proxy, que corre en el host, lo cambia por la contraseña de la copia al
// marcar a ella. La contraseña no entra en el agente.
//
// Lo que se entrega al daemon es el ID de la copia (upstream_machine), no su
// dirección: el daemon la resuelve en CADA conexión y solo si la copia sigue
// corriendo, lista y del mismo dueño que el agente (ver
// internal/machine/copias_db.go). Congelar, parar o borrar la copia corta las
// sesiones vivas; una copia nueva (reset, undo) tiene otro ID y hay que volver
// a hacer attach.
//
// Qué contraseña: la del rol de la aplicación de la copia (dbstate) o, mejor,
// la de un rol creado con `kling db role -ro` (-role): cada agente con su
// rol y sus permisos. Nunca la del golden: la copia la rotó al nacer.
//
// Solo Linux en esta versión: en macOS el daemon lo rechaza con un error claro
// (internal/machine/plataforma_vz.go explica por qué).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	defaultAttachEnv = "PGPASSWORD"
	// attachDomain es el sufijo del nombre por el que el agente llega a la
	// copia: su resolver lo desvía al proxy, que no lo resuelve nunca.
	attachDomain = ".db.internal"
)

var (
	envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	// hostPattern es lo que acepta el proxy como dominio de una credencial
	// (credproxy.ValidarDominio): etiquetas DNS y un dominio de primer nivel
	// de letras.
	hostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	noHostChar  = regexp.MustCompile(`[^a-z0-9-]+`)
)

// attachOpts son los flags de attach.
type attachOpts struct {
	owner, role, env, database, host string
}

func cmdAttach(args []string) error {
	fs, host, owner := newFlags("attach")
	role := fs.String("role", "", "the role the agent connects as, made with kling db role (recommended: a -ro one); default: the application role")
	env := fs.String("env", defaultAttachEnv, "variable of the agent that receives the placeholder")
	database := fs.String("database", "", "the only database the agent may use (default: the copy's)")
	hostName := fs.String("host", "", "name the agent connects to (default: <copy>"+attachDomain+")")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("usage: kling db attach <agent> <copy> [-role R] [-env PGPASSWORD] [-database appdb] [-host H]")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	return a.attach(ctx, pos[0], pos[1], attachOpts{owner: *owner, role: *role, env: *env, database: *database, host: *hostName})
}

// defaultAttachHost es <nombre de la copia>.db.internal, limpio para DNS; si
// del nombre no sale nada válido, db-<id corto>.
func defaultAttachHost(mc *api.Machine) string {
	n := strings.Trim(noHostChar.ReplaceAllString(strings.ToLower(mc.Name), "-"), "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	if h := n + attachDomain; n != "" && hostPattern.MatchString(h) {
		return h
	}
	return "db-" + shortID(mc.ID) + attachDomain
}

func (a *app) attach(ctx context.Context, agentRef, copyRef string, o attachOpts) error {
	if err := validOwner(o.owner); err != nil {
		return err
	}
	if !envPattern.MatchString(o.env) {
		return fmt.Errorf("invalid -env %q: [A-Z_][A-Z0-9_]*", o.env)
	}
	cp, err := a.inspect(ctx, copyRef)
	if err != nil {
		return err
	}
	if err := checkReady(cp, o.owner); err != nil {
		return err
	}
	ag, err := a.inspect(ctx, agentRef)
	if err != nil {
		return err
	}
	if ag.ID == cp.ID {
		return errors.New("the agent and the copy are the same machine: inside a copy use the local socket (see kling db connect)")
	}
	if ag.State != api.StateRunning {
		return fmt.Errorf("agent %s is %s, not running", ag.Name, ag.State)
	}
	if ag.Egress != "allowlist" {
		return fmt.Errorf("agent %s has egress %q: the credential proxy needs -egress allowlist (kling run ... -egress allowlist -allow <domain>)", ag.Name, orDefault(ag.Egress, "none"))
	}

	role, db, err := roleDB(cp.Labels)
	if err != nil {
		return err
	}
	user := role
	readPW := func() (string, error) { return dbstate.ReadPassword(cp.ID) }
	if o.role != "" {
		if err := validRoleName(o.role, role); err != nil {
			return err
		}
		user = o.role
		readPW = func() (string, error) {
			pw, err := dbstate.ReadRolePassword(cp.ID, o.role)
			if errors.Is(err, dbstate.ErrNoPassword) {
				return "", fmt.Errorf("this host has no password for role %s of %s (kling db role %s -ro -name %s creates it)", o.role, cp.Name, cp.Name, o.role)
			}
			return pw, err
		}
	}
	if o.database != "" {
		if !identPattern.MatchString(o.database) {
			return fmt.Errorf("invalid -database %q", o.database)
		}
		db = o.database
	}
	host := o.host
	if host == "" {
		host = defaultAttachHost(cp)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !hostPattern.MatchString(host) {
		return fmt.Errorf("invalid -host %q: a DNS name like copy%s", host, attachDomain)
	}
	pw, err := readPW()
	if err != nil {
		return err
	}

	// Dueño del agente: el daemon exige el mismo kling.db.owner en el agente,
	// en la copia y en la credencial, en cada conexión. Un agente sin dueño se
	// etiqueta; uno de otro dueño no se toca.
	switch got := ag.Labels[labelOwner]; got {
	case o.owner:
	case "":
		if err := a.k.SetLabels(ctx, ag.ID, map[string]string{labelOwner: o.owner}); err != nil {
			return fmt.Errorf("labelling agent %s with %s=%s: %w", ag.Name, labelOwner, o.owner, err)
		}
		fmt.Fprintf(a.stderr, "note: agent %s labelled %s=%s\n", ag.Name, labelOwner, o.owner)
	default:
		return fmt.Errorf("agent %s belongs to owner %q, not %q: attach only joins machines of the same owner", ag.Name, got, o.owner)
	}

	spec := api.CredentialSpec{
		Type: "postgres", Domain: host, Env: o.env, Secret: pw,
		Port: pgPort, User: user, Database: db,
		UpstreamMachine: cp.ID, UpstreamOwner: o.owner, UpstreamTLS: "disable",
	}
	if err := a.k.SetCredential(ctx, ag.ID, spec); err != nil {
		// Un daemon anterior ignora upstream_machine y se queda con un
		// upstream_tls disable sin upstream: falla cerrado, pero el mensaje
		// no diría por qué.
		if strings.Contains(err.Error(), "needs -upstream") {
			return fmt.Errorf("this kindling daemon does not support kling db attach (capability db-attach): update kindling")
		}
		return err
	}
	fmt.Fprintf(a.stdout, "%s  attached to %s as %s on %s (the password stays on the host)\n", shortID(ag.ID), cp.Name, user, db)
	fmt.Fprintf(a.stdout, "      in the agent: host=%s port=%d user=%s dbname=%s sslmode=disable, password = $%s (a placeholder)\n",
		host, pgPort, user, db, o.env)
	fmt.Fprintf(a.stdout, "      checked on every connection: freezing, stopping or removing %s cuts it; kling db detach %s %s\n",
		cp.Name, ag.Name, cp.Name)
	if o.role == "" {
		fmt.Fprintf(a.stderr, "warning: the agent gets the application role %s; for a read-only agent: kling db role %s -ro && kling db attach %s %s -role %s\n",
			role, cp.Name, ag.Name, cp.Name, defaultRoleRO)
	}
	return nil
}

func cmdDetach(args []string) error {
	fs, host, owner := newFlags("detach")
	env := fs.String("env", defaultAttachEnv, "variable the attach used")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("usage: kling db detach <agent> <copy> [-env PGPASSWORD]")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	return a.detach(ctx, pos[0], pos[1], *owner, *env)
}

// reMachineID es un id de máquina completo: detach lo admite aunque la copia
// ya no exista (la borraron sin detach antes).
var reMachineID = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

func (a *app) detach(ctx context.Context, agentRef, copyRef, owner, env string) error {
	if err := validOwner(owner); err != nil {
		return err
	}
	if !envPattern.MatchString(env) {
		return fmt.Errorf("invalid -env %q: [A-Z_][A-Z0-9_]*", env)
	}
	ag, err := a.inspect(ctx, agentRef)
	if err != nil {
		return err
	}
	// Como en attach: solo se toca un agente del mismo dueño, también cuando la
	// copia ya no existe y no hay otra comprobación.
	if got := ag.Labels[labelOwner]; got != owner {
		return fmt.Errorf("agent %s belongs to owner %q, not %q: detach only touches machines of the same owner", ag.Name, got, owner)
	}
	copyID, copyName := copyRef, copyRef
	if cp, err := a.inspect(ctx, copyRef); err == nil {
		if err := owned(cp, owner); err != nil {
			return err
		}
		copyID, copyName = cp.ID, cp.Name
	} else if !reMachineID.MatchString(copyRef) {
		return fmt.Errorf("%w (if the copy is gone, pass its full id, or: kling machine credential -rm %s -env %s)", err, ag.Name, env)
	}
	if err := a.k.RemoveCredential(ctx, ag.ID, env, copyID); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s  detached from %s: %s no longer reaches it, and its open sessions were cut\n", shortID(ag.ID), copyName, env)
	return nil
}
