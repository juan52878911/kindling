package main

// kling db rotate <copia>: contraseña nueva para una copia lista.
//
// Atómico de cara al usuario: o cambian la base y el fichero, o no cambia
// ninguno. El orden es el inverso al de la rotación de up (que escribe antes
// porque, si falla, destruye la copia): aquí la copia sigue viva y debe seguir
// siendo alcanzable con la clave que ya tenía.
//
//  1. la nueva se deja en password.new (la vigente no se toca)
//  2. se cambia el verificador en la base (setVerifier, el mismo mecanismo)
//  3. solo si eso fue bien, password.new pasa a ser password (rename)
//
// Si el paso 2 falla o queda en duda (un plazo vencido puede haberse aplicado),
// se devuelve a la base el verificador de la clave anterior, que sigue en el
// host. Si el paso 3 falla, igual.

import (
	"context"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

func cmdRotate(args []string) error {
	fs, host, owner := newFlags("rotate")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db rotate <copy>")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	mc, err := a.rotateCopy(ctx, pos[0], *owner)
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

// rotateCopy es la operación completa sobre una copia lista y propia.
func (a *app) rotateCopy(ctx context.Context, ref, owner string) (*api.Machine, error) {
	if err := validOwner(owner); err != nil {
		return nil, err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := checkReady(mc, owner); err != nil {
		return nil, err
	}
	role, _, err := roleDB(mc.Labels)
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
	ver, err := newVerifier(pw)
	if err != nil {
		return nil, err
	}
	if err := dbstate.StagePassword(mc.ID, pw); err != nil {
		return nil, fmt.Errorf("staging the new password of %s: %w", mc.Name, err)
	}
	if err := a.setVerifier(ctx, mc.ID, role, ver); err != nil {
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
	ver, err := newVerifier(oldPw)
	if err == nil {
		err = a.setVerifier(ctx, mc.ID, role, ver)
	}
	if err != nil {
		return fmt.Errorf("%w; could not confirm the previous password is still in force (%v): "+
			"if connect stops working, kling db reset %s", cause, err, mc.Name)
	}
	return fmt.Errorf("%w; the previous password is unchanged", cause)
}
