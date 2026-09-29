package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Marca de "esta identidad ya está aplicada", en la RAM de la VM (no en
// /data de Android, no en ningún disco): sobrevive a freeze/thaw con la
// memoria, el dorado nunca la tiene. Es un sha256 del documento, no el
// documento.
const identityMark = stateDir + "/identity.sha256"

const mmdsBase = "http://169.254.169.254"

// runIdentity es el gancho tras restaurar (post-restore.d/10-identity).
func runIdentity() int {
	ctx, cancel := context.WithTimeout(context.Background(), 58*time.Second) // el núcleo corta a los 60
	defer cancel()
	restore := os.Getenv("KLING_RESTORE")
	id, err := fetchPhoneIdentity(ctx, mmdsBase)
	if err != nil {
		fmt.Printf("identity: %v\n", err)
		return 1
	}
	if id == nil {
		fmt.Printf("no identity in MMDS (%s): nothing to apply\n", orQ(restore))
		return 0
	}
	sum := id.digest()
	if b, err := os.ReadFile(identityMark); err == nil && strings.TrimSpace(string(b)) == sum {
		// El mismo documento otra vez (un thaw con el almacén aún lleno): no se
		// regeneran los SSAID en cada descongelación.
		fmt.Println("identity already applied")
		return 0
	}
	t0 := time.Now()
	done, err := applyIdentity(ctx, id)
	if err != nil {
		fmt.Printf("identity: %s\n", redact(err.Error(), id))
		return 1
	}
	_ = os.WriteFile(identityMark, []byte(sum+"\n"), 0o600)
	// Ni un trozo de los valores: esta salida acaba en la consola (kling logs).
	fmt.Printf("identity applied (%s) in %.2fs\n", strings.Join(done, ", "), time.Since(t0).Seconds())
	return 0
}

// redact quita de un mensaje cualquier valor de la identidad.
func redact(msg string, p *phoneIdentity) string {
	vals := []string{p.AndroidID, p.Serial, p.Name, p.SSAIDKey, strings.ToLower(p.SSAIDKey)}
	for _, k := range p.AdbKeys {
		b64, _, _ := strings.Cut(strings.TrimSpace(k), " ")
		vals = append(vals, b64)
	}
	for _, v := range vals {
		if len(v) >= 4 {
			msg = strings.ReplaceAll(msg, v, "***")
		}
	}
	return msg
}

// waitBoot espera sys.boot_completed=1 (leído de memoria).
func waitBoot(ctx context.Context, pid int) error {
	for {
		if v, err := getProp(pid, "sys.boot_completed"); err == nil && v == "1" && procByComm("system_server") > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("android did not finish booting: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// applyIdentity: serie, SSAID, android_id y nombre, claves de adb. Devuelve
// qué se aplicó.
func applyIdentity(ctx context.Context, id *phoneIdentity) ([]string, error) {
	pid, err := findInitPID()
	if err != nil {
		return nil, err
	}
	if err := waitBoot(ctx, pid); err != nil {
		return nil, err
	}
	var done []string
	run := func(timeout time.Duration, stdin []byte, script string, args ...string) error {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var in *bytes.Reader
		if stdin != nil {
			in = bytes.NewReader(stdin)
		}
		var err error
		if in != nil {
			_, err = runIn(c, pid, zygoteEnv(pid), in, script, args...)
		} else {
			_, err = runIn(c, pid, zygoteEnv(pid), nil, script, args...)
		}
		return err
	}

	// 1. ro.serialno (y ro.boot.serialno, de donde init la copió): de solo
	// lectura para setprop, así que se reescribe en la memoria de propiedades.
	// Antes de reiniciar el framework, para que system_server arranque ya con
	// la nueva.
	serialOK := true
	for _, name := range []string{"ro.serialno", "ro.boot.serialno"} {
		if err := setPropInPlace(pid, name, id.Serial); err != nil {
			if name == "ro.serialno" {
				serialOK = false
				fmt.Printf("identity: warning: cannot set %s (%v); the image boots without androidboot.serialno?\n", name, err)
			}
		}
	}
	if serialOK {
		done = append(done, "serial")
	}

	// 2. SSAID por app: la clave de usuario de settings_ssaid.xml es de la que
	// salen TODOS los ANDROID_ID por app (HMAC con la firma de cada una).
	// SettingsProvider la crea la primera vez que una app pide su ANDROID_ID,
	// con un SecureRandom de system_server... cuyo estado (el DRBG de BoringSSL
	// en su memoria) es el mismo en todos los clones de un dorado: resembrar el
	// kernel tras restaurar no lo toca. Así que se para zygote (y con él
	// system_server), se escribe una clave NUESTRA (de MMDS, o de crypto/rand
	// del kernel ya resembrado) y al volver system_server es un proceso nuevo
	// que la lee. La clave va por stdin, como las de adb.
	if id.SSAID == "regen" {
		t := time.Now()
		if err := run(40*time.Second, []byte(id.ssaidXML()), `
			f=/data/system/users/0/settings_ssaid.xml
			cat > /data/system/users/0/.ssaid.new || exit 1
			setprop ctl.stop zygote
			i=0; while pidof system_server >/dev/null && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
			pidof system_server >/dev/null && { echo "system_server did not stop" >&2; exit 1; }
			rm -f "$f" "$f.bak" "$f.fallback"
			# ABX, como lo escribiría Android; si xml2abx falla, el texto vale igual.
			xml2abx /data/system/users/0/.ssaid.new "$f" 2>/dev/null || cp /data/system/users/0/.ssaid.new "$f" || exit 1
			rm -f /data/system/users/0/.ssaid.new
			chown system:system "$f"; chmod 0600 "$f"
			setprop sys.boot_completed 0
			setprop ctl.start zygote`); err != nil {
			return done, fmt.Errorf("ssaid: %v", err)
		}
		// setprop devuelve antes de que init lo aplique: que se vea el 0 o
		// que ya no haya system_server antes de esperar al 1.
		time.Sleep(200 * time.Millisecond)
		if err := waitBoot(ctx, pid); err != nil {
			return done, fmt.Errorf("ssaid: framework restart: %v", err)
		}
		done = append(done, fmt.Sprintf("ssaid regenerated %.1fs", time.Since(t).Seconds()))
	}

	// 3. android_id y nombre visible (como el gancho de bash).
	if err := run(30*time.Second, nil, `
		settings put secure android_id "$1" || exit 1
		[ -z "$2" ] || settings put global device_name "$2" || exit 1
		[ "$(settings get secure android_id)" = "$1" ]`, id.AndroidID, id.Name); err != nil {
		return done, fmt.Errorf("android_id: %v", err)
	}
	done = append(done, "android_id")
	if id.Name != "" {
		done = append(done, "name")
	}

	// 4. adb: las claves de ESTE clon (por stdin, no en argv). Sin claves, el
	// fichero se borra: con ro.adb.secure=1 nadie entra por adb.
	if err := run(15*time.Second, []byte(id.adbKeysFile()), `
		d=/data/misc/adb; mkdir -p $d || exit 1
		cat > $d/adb_keys.new || exit 1
		if [ -s $d/adb_keys.new ]; then
			chown system:shell $d/adb_keys.new; chmod 0640 $d/adb_keys.new; mv $d/adb_keys.new $d/adb_keys
		else
			rm -f $d/adb_keys.new $d/adb_keys
		fi`); err != nil {
		return done, fmt.Errorf("adb keys: %v", err)
	}
	done = append(done, fmt.Sprintf("adb keys %d", len(id.AdbKeys)))
	return done, nil
}
