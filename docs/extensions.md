# Extensiones de kling

`kling` es el único comando que teclea quien usa kindling. El núcleo trae las
microVMs —máquinas, snapshots, red, volúmenes, imágenes— y todo lo demás lo
aporta una **extensión**: un ejecutable `kling-<nombre>` que declara qué
subcomandos añade. `kling mcp import` sigue funcionando igual cuando la parte de
MCP vive en su propio binario, porque es `kling` quien lo encuentra y le pasa el
control.

```
kling mcp import eco        →  kling busca quién sirve "mcp"  →  exec kling-mcp mcp import eco
```

Este documento es el contrato para quien escribe una extensión.

## Qué es una extensión

Un ejecutable llamado `kling-<nombre>`, donde `<nombre>` son minúsculas, dígitos
y guiones. `kling` lo busca, en este orden, y el primero que aparece con un
nombre gana:

1. los directorios de `$KLING_PLUGIN_PATH` (separados por `:`);
2. `<prefijo>/lib/kindling/plugins/`, junto al binario de `kling`;
3. `~/.local/share/kling/plugins/` (o `$XDG_DATA_HOME/kling/plugins/`);
4. el `PATH`.

`kling-bridge` y `kling-guest` acompañan a kindling pero no son extensiones: nunca
se les pide nada.

`kling` solo busca extensiones cuando le hace falta: ante un comando que no es
del núcleo, y en `help`, `completion`, `status`, `config` y `plugins`. `kling ps`
no ejecuta nada de nadie.

## El manifiesto

`kling-<nombre> --kling-manifest` tiene que imprimir su manifiesto en JSON y
salir con 0, en menos de 2 segundos:

```json
{
  "manifest_version": 1,
  "name": "mcp",
  "version": "0.1.0",
  "min_kling": "0.6.0",
  "summary": "hosts MCP servers on demand in microVMs",
  "commands": [
    {
      "name": "mcp",
      "group": "MCP SERVICES",
      "summary": "import, list, verify, heal, link",
      "usage": "  mcp import <service> -image <img>   turns an MCP server into a service\n",
      "subcommands": ["import", "list", "verify", "heal", "link", "unlink"]
    },
    { "name": "gateway", "group": "GATEWAY", "summary": "routes MCP calls to microVMs" }
  ],
  "config": [
    { "key": "memory.service", "type": "string", "help": "MCP service used as usage memory" }
  ],
  "hooks": ["status"]
}
```

| Campo | Para qué |
|---|---|
| `commands[].name` | subcomando de primer nivel que la extensión recibe |
| `commands[].group` | sección de `kling help` donde aparece |
| `commands[].usage` | el bloque que se imprime en la ayuda, con su formato de columnas; vacío = una línea con `name` y `summary` |
| `commands[].subcommands` | para el completado de la shell |
| `config[]` | claves que se tocan con `kling config set <nombre>.<clave> <valor>`; `type` es `string`, `bool`, `int` o `secret` (este último se muestra enmascarado) |
| `hooks` | `status` y/o `up` |
| `min_kling` | versión mínima del núcleo; si el núcleo es más viejo, la extensión se lista con el motivo y no se ejecuta |

Un manifiesto roto, lento o de otra extensión no rompe `kling`: la extensión
aparece en `kling plugins` con su error y ya.

## Cómo se ejecuta

`kling <comando> <args…>` hace `exec` de `kling-<nombre> <comando> <args…>`: la
extensión **reemplaza** al proceso de `kling`. Por eso el código de salida, las
señales y la terminal son los suyos, sin nada en medio (el `3` de
`kling mcp heal`, que interpreta systemd, llega intacto).

El entorno lleva lo que el núcleo sabe de sí mismo:

| Variable | Valor |
|---|---|
| `KLING_PLUGIN_API` | versión del protocolo, hoy `1` |
| `KLING_VERSION` | versión del núcleo |
| `KLING_BIN` | ruta absoluta de `kling`, por si la extensión quiere llamarle |
| `KLING_CONFIG` | ruta de `config.json` |

Lo demás —`KLING_HOST`, `KLING_SOCKET`, el flag `-H`— lo resuelve la extensión
con `pkg/config`, con la misma precedencia que el núcleo:
`-H > $KLING_HOST > contexto activo > socket local`.

Los comandos del núcleo ganan siempre. Una extensión que declare `ps` no lo
recibe; `kling plugins` lo marca como ignorado. Entre dos extensiones, gana la
primera que se encuentra.

## Ganchos

- **`status`**: `kling status` imprime lo del núcleo (endpoint, daemon, KVM,
  firecracker) y después ejecuta `kling-<nombre> --kling-hook status <args>`, que
  imprime sus líneas ya alineadas. Con `-json` el gancho recibe `-json` y escribe
  un objeto, que `kling status -json` pone bajo `extensions.<nombre>`. Los flags
  que el núcleo no conoce (`-gateway`, por ejemplo) llegan tal cual al gancho.
- **`up`**: reservado para que la extensión compruebe o arranque lo suyo en
  `kling up`.

Un gancho tiene 5 segundos. Si falla o tarda, `kling` imprime una línea de aviso
y sigue: un gancho nunca rompe la salida del núcleo.

## Escribirla en Go

`pkg/plugin.Main` implementa todo el contrato:

```go
package main

import (
	"io"

	"github.com/juan52878911/kindling/pkg/plugin"
)

func main() {
	plugin.Main(plugin.Manifest{
		Name: "hello", Version: "0.1.0",
		Commands: []plugin.Command{{Name: "hello", Group: "HELLO", Summary: "says hello"}},
		Hooks:    []string{plugin.HookStatus},
	}, map[string]func([]string) error{
		"hello": func(args []string) error { println("hola"); return nil },
	}, map[string]func([]string, io.Writer) error{
		plugin.HookStatus: func(_ []string, w io.Writer) error {
			_, err := io.WriteString(w, "hello:        ✓ fine\n")
			return err
		},
	})
}
```

Para pedir un código de salida concreto, el comando devuelve
`&plugin.ExitError{Code: 3, Err: err}`. La extensión también se puede usar a
mano: `kling-hello hello` hace lo mismo que `kling hello`.

## Lo que el núcleo ofrece a una extensión

Todo por el API del daemon (`pkg/api`), nunca por dentro:

| Pieza | Para qué |
|---|---|
| **Anotaciones de snapshot** — `PUT/DELETE /snapshots/{name}/annotations/{key}` | datos de la extensión que viven con un snapshot (kindling-mcp guarda ahí su catálogo y su salud) |
| **Store** — `GET/PUT/DELETE /store/{ns}/{key}` | estado global de la extensión en el host del daemon (kindling-mcp guarda ahí sus servidores externos enlazados) |
| **Constructores de imágenes** — `POST /images` con `builder` y `spec` | construir imágenes a su manera; el constructor es un ejecutable de root en `/usr/local/lib/kindling/builders/<nombre>` |
| **Ficheros en imágenes** — `GET/PUT /images/{name}/files` | leer o poner al día un fichero dentro de una imagen ya construida |
| **Proxy al invitado** — `POST /machines/{ref}/guest` | hablar con el servidor de dentro de una microVM; ruta, cabeceras y tamaño los decide la extensión |
| `pkg/scheduler` | despertar, congelar por inactividad, réplicas, afinidad de sesión, precalentado y cuotas, con ganchos para lo propio |
| `pkg/guest` | el agente de invitado genérico (exec, volúmenes, MMDS, DNS), para embeberlo en el PID 1 de sus imágenes |

`GET /info` devuelve `capabilities`: la extensión comprueba ahí que el daemon
tiene lo que necesita antes de usarlo, en vez de deducirlo de la versión.

La tabla completa de rutas está en [`api.md`](api.md).
