package main

// `kling sbx tenant …` y `kling sbx key …`: administración de inquilinos y
// claves del gateway (token de admin). Solo hablan con /v1/tenants.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"
)

type tenantJSON struct {
	ID       string         `json:"id"`
	Origen   string         `json:"source"`
	SoloLeer bool           `json:"read_only"`
	Admin    bool           `json:"admin"`
	Topes    map[string]int `json:"limits"`
	Claves   []struct {
		ID       string     `json:"id"`
		Prefijo  string     `json:"prefix"`
		Caduca   *time.Time `json:"expires"`
		Revocada *time.Time `json:"revoked"`
		Uso      *time.Time `json:"last_used"`
	} `json:"keys"`
}

func sbxTenant(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx tenant <ls|show|add|set|rm> [...]")
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	sub, resto := args[0], args[1:]
	switch sub {
	case "ls", "list":
		var r struct {
			Tenants []tenantJSON `json:"tenants"`
		}
		if err := c.llamar(ctx, http.MethodGet, "/v1/tenants", nil, &r); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "ID\tSOURCE\tADMIN\tKEYS")
		for _, t := range r.Tenants {
			vivas := 0
			for _, k := range t.Claves {
				if k.Revocada == nil {
					vivas++
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%v\t%d\n", t.ID, t.Origen, t.Admin, vivas)
		}
		return tw.Flush()
	case "show":
		if len(resto) != 1 {
			return errors.New("usage: kling sbx tenant show <id>")
		}
		var t json.RawMessage
		if err := c.llamar(ctx, http.MethodGet, "/v1/tenants/"+url.PathEscape(resto[0]), nil, &t); err != nil {
			return err
		}
		return imprimirJSON(t)
	case "add", "set":
		fs := flag.NewFlagSet("sbx tenant "+sub, flag.ExitOnError)
		admin := fs.Bool("admin", false, "tenant can manage tenants and keys, and see /v1/hosts")
		topes := map[string]*int{}
		for _, k := range []string{"max_sandboxes", "max_per_template", "max_shells", "max_processes", "max_sessions",
			"max_follow", "max_mem_mib", "max_vcpus", "max_mem_total_mib", "max_batch", "max_volumes"} {
			topes[k] = fs.Int(k, -1, "limit "+k+" (0 = gateway default)")
		}
		// El nombre va antes de las banderas: `add ana -max_sandboxes 5`.
		if len(resto) == 0 {
			return fmt.Errorf("usage: kling sbx tenant %s <id> [-admin] [-max_sandboxes N ...]", sub)
		}
		id := resto[0]
		if err := fs.Parse(resto[1:]); err != nil {
			return err
		}
		lim := map[string]int{}
		for k, p := range topes {
			if *p >= 0 {
				lim[k] = *p
			}
		}
		var out json.RawMessage
		if sub == "add" {
			cuerpo := map[string]any{"name": id, "admin": *admin, "limits": lim}
			if err := c.llamar(ctx, http.MethodPost, "/v1/tenants", cuerpo, &out); err != nil {
				return err
			}
		} else {
			cuerpo := map[string]any{}
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "admin" {
					cuerpo["admin"] = *admin
				}
			})
			if len(lim) > 0 {
				cuerpo["limits"] = lim
			}
			if err := c.llamar(ctx, http.MethodPatch, "/v1/tenants/"+url.PathEscape(id), cuerpo, &out); err != nil {
				return err
			}
		}
		return imprimirJSON(out)
	case "rm", "remove":
		if len(resto) != 1 {
			return errors.New("usage: kling sbx tenant rm <id>")
		}
		return c.llamar(ctx, http.MethodDelete, "/v1/tenants/"+url.PathEscape(resto[0]), nil, nil)
	}
	return fmt.Errorf("unknown subcommand %q: use ls, show, add, set or rm", sub)
}

func sbxKey(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: kling sbx key <new <tenant> [-expires RFC3339] | rotate <tenant> <key-id> [-grace 10m] | rm <tenant> <key-id>>")
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	sub, tenant := args[0], url.PathEscape(args[1])
	base := "/v1/tenants/" + tenant + "/keys"
	switch sub {
	case "new":
		fs := flag.NewFlagSet("sbx key new", flag.ExitOnError)
		caduca := fs.String("expires", "", "expiry as RFC3339 (default: never)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		cuerpo := map[string]any{}
		if *caduca != "" {
			cuerpo["expires_at"] = *caduca
		}
		var r respuestaClave
		return imprimirClave(c.llamar(ctx, http.MethodPost, base, cuerpo, &r), &r)
	case "rotate":
		if len(args) < 3 {
			return errors.New("usage: kling sbx key rotate <tenant> <key-id> [-grace 10m]")
		}
		fs := flag.NewFlagSet("sbx key rotate", flag.ExitOnError)
		gracia := fs.Duration("grace", 10*time.Minute, "how long the old key keeps working (0 = revoke now)")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		cuerpo := map[string]any{"grace_minutes": int(*gracia / time.Minute)}
		var r respuestaClave
		return imprimirClave(c.llamar(ctx, http.MethodPost, base+"/"+url.PathEscape(args[2])+"/rotate", cuerpo, &r), &r)
	case "rm", "revoke":
		if len(args) != 3 {
			return errors.New("usage: kling sbx key rm <tenant> <key-id>")
		}
		return c.llamar(ctx, http.MethodDelete, base+"/"+url.PathEscape(args[2]), nil, nil)
	}
	return fmt.Errorf("unknown subcommand %q: use new, rotate or rm", sub)
}

type respuestaClave struct {
	ID      string `json:"id"`
	Prefijo string `json:"prefix"`
	Clave   string `json:"key"`
}

// imprimirClave escribe la clave nueva en stdout (y nada más, para poder
// capturarla con $(...)); el id va a stderr. La clave no se vuelve a poder leer.
func imprimirClave(err error, r *respuestaClave) error {
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "key id: %s (shown once: store it now)\n", r.ID)
	fmt.Println(r.Clave)
	return nil
}
