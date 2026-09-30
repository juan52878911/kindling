package main

// Copias de reserva de kling db branch: una rama nueva sin esperar al fork.
//
// Ramificar la copia viva del padre cuesta ~2 s (pausarla y volcar su memoria:
// 1 GiB en el golden de Postgres) y es lo que esperaba `git checkout -b`. Una
// RESERVA es ese mismo fork hecho antes, en segundo plano, y guardado congelado
// (0 RAM): cuando llega la rama nueva, se la queda (le cambia las etiquetas),
// se descongela en ~40 ms y ya está.
//
// Corrección. La rama nueva tiene que empezar con los datos que el padre tiene
// AHORA, no los de cuando se sacó la reserva. Por eso cada reserva lleva la
// HUELLA del padre en ese momento (fingerprintSQL) y solo se adopta si la del
// padre sigue siendo la misma; si no, la rama sale del fork de siempre y la
// reserva se tira. La huella cambia con cualquier escritura: toda transacción
// que escribe (en tablas con o sin WAL, temporales, DDL) recibe un xid, y
// pg_current_snapshot() cambia en cuanto una empieza o acaba; nextval() puede
// no escribir WAL ni recibir xid, así que van también los valores de las
// secuencias; y la configuración que vive fuera de las tablas
// (postgresql.auto.conf, pg_hba.conf) por su md5. Una transacción a medias en
// el padre al sacar la reserva no se cuela: al acabar cambia la instantánea.
//
// Cuándo se saca. Solo la rama por defecto (el padre que usa el gancho) tiene
// reserva, y solo mientras NINGÚN worktree la tiene activa: sacarla pausa la
// copia ~1-2 s, y eso no se le hace a la base que alguien está usando. En la
// práctica, al salir de la rama por defecto (settle, antes de congelarla). Una
// reserva de un padre congelado no se comprueba hasta adoptarla (el padre se
// descongela para leer su huella).
//
// Seguridad. Una reserva es una copia preparada como cualquier otra (clave
// propia, estrenada al prepararla, solo en este host); al adoptarla no se
// genera otra: nunca estuvo en uso ni la vio nadie. Solo Postgres: en MySQL la
// rama nueva sale siempre del fork.
//
// KLING_DB_BRANCH_SPARE=0 las desactiva (cada reserva ocupa en disco su volcado
// de memoria, ~200 MiB con el golden pg).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const spareEnv = "KLING_DB_BRANCH_SPARE"

func sparesEnabled() bool { return os.Getenv(spareEnv) != "0" }

// fingerprintSQL es la huella de una copia Postgres (ver arriba). %[1]s es la
// base de la aplicación (identPattern). Solo lee: no asigna xid ni escribe.
const fingerprintSQL = `\connect %[1]s
SELECT md5(concat_ws('|', pg_current_snapshot()::text,
  (SELECT string_agg(schemaname || '.' || sequencename || '=' || coalesce(last_value::text, '-'), ',' ORDER BY schemaname, sequencename) FROM pg_sequences),
  md5(pg_read_file('postgresql.auto.conf')),
  md5(pg_read_file(current_setting('hba_file')))));
`

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// fingerprint es la huella de la copia mc, que tiene que estar en marcha.
func (a *app) fingerprint(ctx context.Context, mc *api.Machine) (string, error) {
	defer a.tr.span("phase fingerprint")()
	_, db, err := roleDB(mc.Labels)
	if err != nil {
		return "", err
	}
	out, err := a.k.Run(ctx, strings.NewReader(fmt.Sprintf(fingerprintSQL, db)), "exec", "-i", "-timeout", "10s", mc.ID, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
	if err != nil {
		return "", fmt.Errorf("reading the state of %s: %w", mc.Name, err)
	}
	fp := lastLine(string(out))
	if !fingerprintPattern.MatchString(fp) {
		return "", fmt.Errorf("reading the state of %s: unexpected answer", mc.Name)
	}
	return fp, nil
}

// spareCandidates son las reservas de cands que se podrían adoptar: listas,
// propias, con clave aquí, Postgres y con huella.
func spareCandidates(cands []*api.Machine, owner string) []*api.Machine {
	var out []*api.Machine
	for _, c := range cands {
		if usable(c, owner) && engineOf(c.Labels) == enginePostgres && fingerprintPattern.MatchString(c.Labels[labelSpareFP]) {
			out = append(out, c)
		}
	}
	return out
}

// adoptSpare da a la rama (sus etiquetas van en extra) la reserva de parent
// cuya huella es la de parent ahora, o nil si no hay ninguna. Se llama con el
// cerrojo del repo tomado. thawed dice si hubo que descongelar al padre para
// leer su huella (quien llama decide si lo vuelve a congelar).
func (a *app) adoptSpare(ctx context.Context, parent *api.Machine, cands []*api.Machine, owner string, extra [][2]string) (sp *api.Machine, thawed bool, err error) {
	if !sparesEnabled() || engineOf(parent.Labels) != enginePostgres {
		return nil, false, nil
	}
	ok := spareCandidates(cands, owner)
	if len(ok) == 0 {
		return nil, false, nil
	}
	defer a.tr.span("phase adopt")()
	switch parent.State {
	case api.StateRunning:
	case api.StateWarm, api.StatePaused:
		cur, err := a.k.Thaw(ctx, parent.ID)
		if err != nil {
			return nil, false, err
		}
		parent, thawed = cur, true
	default:
		return nil, false, nil
	}
	fp, err := a.fingerprint(ctx, parent)
	if err != nil {
		return nil, thawed, err
	}
	for _, c := range ok {
		if c.Labels[labelSpareFP] != fp {
			continue
		}
		labels := map[string]string{labelSpare: "", labelSpareFP: ""}
		for _, l := range extra {
			labels[l[0]] = l[1]
		}
		if err := a.k.SetLabels(ctx, c.ID, labels); err != nil {
			return nil, thawed, err
		}
		for k, v := range labels {
			c.Labels[k] = v
		}
		return c, thawed, nil
	}
	return nil, thawed, nil
}

// settleSpare mantiene la reserva de la rama por defecto (lo llama settle, en
// segundo plano): tira las que ya no valen y saca una nueva si no queda
// ninguna y la rama no está activa en ningún worktree. Todo lo que toca una
// reserva que ya existe (borrarla, congelarla) va con el cerrojo del repo y
// releyéndola: un -switch pudo adoptarla entre tanto, y entonces ya es la copia
// de una rama.
func (a *app) settleSpare(ctx context.Context, ri *repoInfo, owner string) error {
	if !sparesEnabled() {
		return nil
	}
	defer a.tr.span("phase spare")()
	// Una sola a la vez por repo: dos settle seguidos no sacan dos reservas.
	un, err := dbstate.Lock(ctx, "spare-"+ri.repo, 0, nil)
	if errors.Is(err, dbstate.ErrLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer un()

	copies, spares, err := a.repoState(ctx, ri, owner)
	if err != nil {
		return err
	}
	// Las reservas de ramas que ya no tienen copia no sirven para nada.
	for k, l := range spares {
		if len(copies[k]) == 0 {
			for _, s := range l {
				a.dropSpare(ctx, ri, s.ID, k, owner)
			}
		}
	}
	key := branchKey(a.defaultBranch(ctx))
	parent, err := one(copies, key)
	if err != nil || parent == nil || !usable(parent, owner) || engineOf(parent.Labels) != enginePostgres {
		return err
	}
	// Sin el cerrojo del repo a partir de aquí (el fork tarda segundos): si un
	// checkout activa la rama mientras tanto, su base se pausa ~1-2 s una vez.
	inUse, err := a.worktreeBranches(ctx)
	if err != nil || inUse[key] {
		return err
	}
	cands := spares[key]
	fp := ""
	switch parent.State {
	case api.StateRunning:
		if fp, err = a.fingerprint(ctx, parent); err != nil {
			return err
		}
		kept := false
		for _, c := range cands {
			if !kept && c.Labels[labelSpareFP] == fp && len(spareCandidates([]*api.Machine{c}, owner)) == 1 {
				kept = true
				continue
			}
			a.dropSpare(ctx, ri, c.ID, key, owner)
		}
		if kept {
			return nil
		}
	case api.StateWarm, api.StatePaused:
		if len(cands) > 0 {
			return nil // su huella se mira al adoptarla
		}
		if parent, err = a.k.Thaw(ctx, parent.ID); err != nil {
			return err
		}
		if fp, err = a.fingerprint(ctx, parent); err != nil {
			return err
		}
	default:
		return nil
	}

	fmt.Fprintf(a.stderr, "preparing a spare copy of %s for the next new branch\n", parent.Name)
	extra := [][2]string{{labelRepo, ri.repo}, {labelBranch, ""}, {labelSpare, key}, {labelSpareFP, fp}}
	cs, err := a.forkWith(ctx, parent.ID, 1, owner, extra)
	if err != nil {
		return err
	}
	// Congelada no gasta RAM; si alguien la adoptó ya, es suya y sigue en marcha.
	return a.withSpare(ctx, ri, cs[0].ID, key, owner, func(sp *api.Machine) error {
		if sp.State != api.StateRunning {
			return nil
		}
		_, err := a.k.Run(ctx, nil, "freeze", sp.ID)
		return err
	})
}

// withSpare ejecuta fn con el cerrojo del repo tomado sobre la máquina id,
// releída, si sigue siendo una reserva de la rama key, del repo y de owner. Si
// ya no lo es (la adoptó una rama) o no existe, no hace nada.
func (a *app) withSpare(ctx context.Context, ri *repoInfo, id, key, owner string, fn func(*api.Machine) error) error {
	un, err := a.lockRepo(ctx, ri)
	if err != nil {
		return err
	}
	defer un()
	cur, err := a.inspect(ctx, id)
	if err != nil {
		return nil
	}
	if cur.Labels[labelBranch] != "" || cur.Labels[labelSpare] != key || !ri.is(cur.Labels[labelRepo]) || owned(cur, owner) != nil {
		return nil
	}
	return fn(cur)
}

// dropSpare borra una reserva (y su clave) si sigue siéndolo. Mejor esfuerzo:
// una que no se pudo borrar se vuelve a intentar en el siguiente settle.
func (a *app) dropSpare(ctx context.Context, ri *repoInfo, id, key, owner string) {
	err := a.withSpare(ctx, ri, id, key, owner, func(sp *api.Machine) error { return a.remove(ctx, sp) })
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove the spare copy %s: %v\n", shortID(id), err)
	}
}
