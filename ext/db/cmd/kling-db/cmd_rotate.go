package main

// kling db rotate <copia>: contraseña nueva para una copia lista.
//
// Atómico de cara al usuario: o cambian la base y el fichero, o no cambia
// ninguno. El orden es el inverso al de la rotación de up (que escribe antes
// porque, si falla, destruye la copia): aquí la copia sigue viva y debe seguir
// siendo alcanzable con la clave que ya tenía.
//
//  1. la nueva se deja en password.new (la vigente no se toca)
//  2. se cambia el verificador en la base (setVerifier, el mismo mecanismo;
//     en MySQL, el hash de mysql_native_password con setMySQLHash; en Redis,
//     el SHA-256 del usuario ACL con setRedisHash)
//  3. solo si eso fue bien, password.new pasa a ser password (rename)
//
// Si el paso 2 falla o queda en duda (un plazo vencido puede haberse aplicado),
// se devuelve a la base el verificador de la clave anterior, que sigue en el
// host. Si el paso 3 falla, igual.
//
// Si el proceso muere entre el paso 2 y el 3, la base tiene la clave de
// password.new y el host sigue con la anterior. kling db rotate otra vez lo
// arregla con una clave nueva; kling db rotate -resume termina la rotación
// interrumpida con la de password.new. connect y sql lo avisan.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

func cmdRotate(args []string) error {
	fs, host, owner := newFlags("rotate")
	resume := fs.Bool("resume", false, "finish an interrupted rotation with the password it left staged, instead of a new one")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db rotate [-resume] <copy>")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	rotate := a.rotateCopy
	if *resume {
		rotate = a.resumeRotate
	}
	mc, err := rotate(ctx, pos[0], *owner)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s  password rotated  (machine %s)\n", mc.Name, shortID(mc.ID))
	fmt.Fprintf(a.stdout, "  the new one is in %s; open sessions keep working until they reconnect\n", passwordPathOrID(mc.ID))
	return nil
}

func passwordPathOrID(id string) string {
	if p, err := dbstate.PasswordPath(id); err == nil {
		return p
	}
	return id
}

// rotateTarget es la copia lista y propia que se va a rotar, y su rol.
func (a *app) rotateTarget(ctx context.Context, ref, owner string) (*api.Machine, string, error) {
	if err := validOwner(owner); err != nil {
		return nil, "", err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	if err := requireEngine(mc, "rotate", enginePostgres, engineMySQL, engineRedis, engineFerret, engineClickHouse, engineGarage); err != nil {
		return nil, "", err
	}
	if err := checkReady(mc, owner); err != nil {
		return nil, "", err
	}
	role, _, err := roleDB(mc.Labels)
	if err != nil {
		return nil, "", err
	}
	return mc, role, nil
}

// resumeRotate termina una rotación interrumpida: vuelve a poner en la base
// la clave de password.new (desde el administrador del invitado, sin pedir la
// anterior; repetirlo no hace daño) y solo entonces la confirma en el host.
// Si falla, password.new se queda para otro intento.
func (a *app) resumeRotate(ctx context.Context, ref, owner string) (*api.Machine, error) {
	mc, role, err := a.rotateTarget(ctx, ref, owner)
	if err != nil {
		return nil, err
	}
	pw, err := dbstate.ReadStaged(mc.ID)
	if errors.Is(err, dbstate.ErrNoPassword) {
		return nil, fmt.Errorf("%s has no interrupted rotation to resume (kling db rotate %s makes a new password)", mc.Name, mc.Name)
	}
	if err != nil {
		return nil, err
	}
	if err := a.setPassword(ctx, mc, role, pw); err != nil {
		return nil, fmt.Errorf("resuming the rotation of %s: %w; nothing changed on this host: retry, or kling db rotate %s for a new password", mc.Name, err, mc.Name)
	}
	if err := dbstate.CommitStaged(mc.ID); err != nil {
		return nil, fmt.Errorf("storing the new password of %s: %w; retry kling db rotate -resume %s", mc.Name, err, mc.Name)
	}
	return mc, nil
}

// warnInterruptedRotate avisa de una rotación que murió a medias: la base
// puede tener ya la clave nueva y el host la anterior.
func (a *app) warnInterruptedRotate(mc *api.Machine) {
	if hasPassword(engineOf(mc.Labels)) && dbstate.HasStaged(mc.ID) {
		fmt.Fprintf(a.stderr, "warning: a kling db rotate of %s was interrupted; if the password is refused, run kling db rotate -resume %s (it keeps the data)\n", mc.Name, mc.Name)
	}
}

// rotateCopy es la operación completa sobre una copia lista y propia.
func (a *app) rotateCopy(ctx context.Context, ref, owner string) (*api.Machine, error) {
	mc, role, err := a.rotateTarget(ctx, ref, owner)
	if err != nil {
		return nil, err
	}
	oldPw, err := dbstate.ReadPassword(mc.ID)
	if err != nil {
		return nil, err
	}
	pw, err := generatePassword()
	if err != nil {
		return nil, err
	}
	if pgBacked(engineOf(mc.Labels)) {
		// Postgres: el verificador antes de tocar nada (si falla, nada cambió).
		if _, err := newVerifier(pw); err != nil {
			return nil, err
		}
	}
	if err := dbstate.StagePassword(mc.ID, pw); err != nil {
		return nil, fmt.Errorf("staging the new password of %s: %w", mc.Name, err)
	}
	if err := a.setPassword(ctx, mc, role, pw); err != nil {
		dbstate.DiscardStaged(mc.ID)
		return nil, a.keepOld(mc, role, oldPw, err)
	}
	if err := dbstate.CommitStaged(mc.ID); err != nil {
		dbstate.DiscardStaged(mc.ID)
		return nil, a.keepOld(mc, role, oldPw, fmt.Errorf("storing the new password of %s: %w", mc.Name, err))
	}
	return mc, nil
}

// keepOld devuelve a la base el verificador de la clave anterior (que sigue en
// el host) y arma el error: la base pudo haber aplicado el cambio aunque el
// intento fallara. Con contexto propio: un Ctrl-C no debe impedir deshacer.
func (a *app) keepOld(mc *api.Machine, role, oldPw string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := a.setPassword(ctx, mc, role, oldPw)
	if err != nil {
		// rotate otra vez pone la clave desde el administrador del invitado,
		// sin pedir la vieja, y deja base y host de acuerdo: reset tiraría los
		// datos de la copia. Vale también tras una rotación muerta a medias
		// (el password.new que quede lo pisa la siguiente).
		return fmt.Errorf("%w; could not confirm the previous password is still in force (%v): "+
			"if connect stops working, run kling db rotate %s again (it keeps the data)", cause, err, mc.Name)
	}
	return fmt.Errorf("%w; the previous password is unchanged", cause)
}
