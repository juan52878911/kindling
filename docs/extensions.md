# Extensiones de kling: escribe la tuya en 10 minutos

`kling` es el único comando que teclea quien usa kindling. El núcleo trae las
microVMs —máquinas, snapshots, red, volúmenes, imágenes— y todo lo demás lo
aporta una **extensión**: un ejecutable `kling-<nombre>` que declara qué
subcomandos añade. `kling` lo encuentra, lo pone en su ayuda y en el completado,
y cuando alguien teclea uno de esos subcomandos le pasa el control.

```
kling mcp import eco   →   kling busca quién sirve "mcp"   →   exec kling-mcp mcp import eco
```

Las extensiones oficiales viven en este mismo repositorio y salen en cada
release: `kling plugins install mcp` y `kling plugins install sandbox`. Este
documento enseña a escribir otra; el ejemplo es
[`examples/hello-extension`](../examples/hello-extension). (La demo de
domótica de [`examples/domotica`](../examples/domotica) ya no es una extensión:
es un programa aparte, `kindling-domotica`, que usa kindling sin añadirle
subcomandos.)

## 1. El código (2 minutos)

[`examples/hello-extension/main.go`](../examples/hello-extension/main.go),
recortado a lo esencial:

```go
package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/juan52878911/kindling/pkg/plugin"
)

var manifest = plugin.Manifest{
	Name:     "hello",
	Version:  "0.1.0",
	MinKling: "0.13.0",
	Summary:  "the smallest kling extension",
	Commands: []plugin.Command{{
		Name:    "hello",
		Group:   "EXAMPLES",
		Summary: "prints a greeting",
		Usage:   "  hello [-name N]                    prints a greeting (hello.greeting changes it)\n",
	}},
	Config: []plugin.ConfigKey{
		{Key: "greeting", Type: "string", Help: "word used by `kling hello` (default: hello)"},
	},
	Hooks: []string{plugin.HookStatus},
}

func main() {
	plugin.Main(manifest, map[string]func([]string) error{
		"hello": cmdHello,
	}, map[string]func([]string, io.Writer) error{
		plugin.HookStatus: hookStatus,
	})
}

func cmdHello(args []string) error {
	fs := flag.NewFlagSet("hello", flag.ContinueOnError)
	name := fs.String("name", "world", "who to greet")
	if err := fs.Parse(args); err != nil {
		return &plugin.ExitError{Code: 2, Err: err}
	}
	fmt.Printf("%s, %s!\n", greeting(), *name)
	return nil
}
```

`plugin.Main` implementa todo el protocolo: responde a `--kling-manifest`,
a `--kling-hook <gancho>`, despacha los comandos del manifiesto y sale con el
código que toca. `greeting()` y `hookStatus` están en el fichero completo.

Solo depende de la biblioteca estándar y de `pkg/plugin` y `pkg/config` del
núcleo, que tampoco tiene dependencias. Para empezar la tuya fuera de este repo:

```sh
mkdir kling-hola && cd kling-hola
go mod init example.com/kling-hola
cp <kindling>/examples/hello-extension/main.go .
go get github.com/juan52878911/kindling@v0.13.0
```

## 2. Compilar e instalar (1 minuto)

Una extensión instalada es un ejecutable en el **directorio de extensiones**:

1. el primer directorio de `$KLING_PLUGIN_PATH`, si está definida;
2. si no, `$XDG_DATA_HOME/kling/plugins`;
3. si no, `~/.local/share/kling/plugins`.

```sh
go build -o ~/.local/share/kling/plugins/kling-hello ./examples/hello-extension
```

No hay que registrar nada: el nombre del fichero es el registro.

## 3. Probarla (2 minutos)

```sh
kling plugins ls                 # hello  0.1.0  ok  ~/.local/share/kling/plugins/kling-hello
kling hello -name Ada            # hello, Ada!
kling config set hello.greeting hola
kling hello                      # hola, world!
kling help hello                 # el bloque "usage" del manifiesto
kling status                     # incluye la línea del gancho de status
source <(kling completion zsh)   # recarga el completado: ahora ofrece "hello"
```

`kling doctor` también la revisa: si su manifiesto no valida o pide un
`min_kling` mayor que el núcleo, aparece con ✗ y el motivo. Y se puede apagar
sin desinstalarla: `kling plugins disable hello` (sale como `disabled` en
`kling plugins ls` y deja de recibir comandos) y `kling plugins enable hello`.

La extensión funciona también sola, sin `kling` delante: `kling-hello hello`
hace lo mismo que `kling hello`, y `kling-hello` sin argumentos imprime su ayuda.

### Un test del manifiesto

[`main_test.go`](../examples/hello-extension/main_test.go) ejecuta el propio
binario de test como si fuera la extensión, pide `--kling-manifest`, comprueba
que es JSON, que `Validate()` lo acepta (es lo que hará `kling` al descubrirla)
y que coincide con la variable `manifest`. Cópialo: un manifiesto roto no rompe
`kling`, pero tu extensión desaparece de la ayuda sin que nadie lo note.

```sh
go test ./examples/hello-extension
```

## 4. El manifiesto, campo a campo

`kling-<nombre> --kling-manifest` imprime el manifiesto en JSON y sale con 0 en
menos de 2 segundos. Uno parecido al de la extensión de MCP, recortado:

```json
{
  "manifest_version": 1,
  "name": "mcp",
  "version": "0.13.0",
  "min_kling": "0.6.0",
  "summary": "hosts MCP servers on demand in microVMs",
  "commands": [
    {
      "name": "mcp",
      "group": "MCP SERVICES",
      "summary": "import, list, verify, heal, link",
      "usage": "  mcp import <service> -image <img>   turns an MCP server into a service\n",
      "subcommands": ["import", "list", "verify", "heal", "link", "unlink"],
      "machine_args": ["verify"]
    }
  ],
  "config": [
    { "key": "memory.service", "type": "string", "help": "MCP service used as usage memory" }
  ],
  "hooks": ["status"],
  "units": ["kling-gateway.service", "kling-heal.timer"],
  "companions": ["kling-bridge"]
}
```

| Campo | Obligatorio | Para qué |
|---|---|---|
| `manifest_version` | sí | formato del manifiesto; hoy `1`. `plugin.Main` lo rellena si lo dejas a cero |
| `name` | sí | `[a-z][a-z0-9-]{0,31}`; tiene que coincidir con el `<nombre>` de `kling-<nombre>` |
| `version` | sí | la de la extensión; sale en `kling plugins ls` y `kling doctor` |
| `min_kling` | no | versión mínima del núcleo. Si el núcleo es más viejo, la extensión se lista con el motivo y no se ejecuta |
| `summary` | no | una línea para `kling plugins ls` y la cabecera de su ayuda |
| `commands[].name` | sí | subcomando de primer nivel que recibe; mismo patrón que `name` |
| `commands[].group` | no | sección de `kling help` donde aparece (`MCP SERVICES`, `EXAMPLES`…) |
| `commands[].summary` | no | la línea de la ayuda si no hay `usage` |
| `commands[].usage` | no | el bloque que imprimen `kling help` y `kling help <cmd>`, ya con su formato de columnas |
| `commands[].subcommands` | no | alimentan el completado de la shell |
| `commands[].machine_args` | no | tras qué subcomandos el completado ofrece ids de máquinas (`""` = tras el comando mismo) |
| `config[]` | no | claves que se tocan con `kling config set <nombre>.<clave> <valor>` y se guardan en `config.json` bajo `extensions.<nombre>.<clave>`. `type` es `string`, `bool`, `int` o `secret` (se muestra enmascarado) |
| `hooks` | no | `status` y/o `up` (ver más abajo) |
| `units` | no | unidades de systemd que la extensión instala en el host del daemon; `kling up` las arranca si están instaladas |
| `companions` | no | ejecutables que se instalan **junto a** la extensión y no son extensiones: `kling plugins install` los baja de la misma release, con la misma verificación, y `kling plugins rm` los borra con ella. La de MCP declara `kling-bridge` |

Un manifiesto roto, lento o que dice ser de otra extensión no rompe `kling`: la
extensión aparece en `kling plugins ls` con `STATUS error: …` y ya.

### Códigos de salida

`kling <comando>` hace `exec` de la extensión, que **reemplaza** al proceso de
`kling`: el código de salida, las señales y la terminal son los suyos. Con
`plugin.Main`: 0 si el comando devuelve `nil`, 1 con cualquier error, 2 para un
comando desconocido, y lo que quieras con `&plugin.ExitError{Code: n, Err: err}`
(el 3 de `kling mcp heal`, que systemd interpreta, llega intacto).

### Entorno

| Variable | Valor |
|---|---|
| `KLING_PLUGIN_API` | versión del protocolo, hoy `1` |
| `KLING_VERSION` | versión del núcleo |
| `KLING_BIN` | ruta absoluta de `kling`, por si la extensión quiere llamarle |
| `KLING_CONFIG` | ruta de `config.json` |

`KLING_HOST`, `KLING_SOCKET` y el flag `-H` los resuelve la extensión con
`pkg/config`, con la misma precedencia que el núcleo:
`-H > $KLING_HOST > contexto activo > socket local`.

### Ganchos

- **`status`**: `kling status` imprime lo del núcleo y después ejecuta
  `kling-<nombre> --kling-hook status <args>`, que imprime sus líneas ya
  alineadas. Con `-json` el gancho recibe `-json` y escribe un objeto, que
  `kling status -json` pone bajo `extensions.<nombre>`. Los flags que el núcleo
  no conoce llegan tal cual al gancho.
- **`up`**: para que la extensión compruebe o arranque lo suyo en `kling up`.

Un gancho tiene 5 segundos. Si falla o tarda, `kling` imprime una línea de aviso
y sigue.

### Dónde busca `kling`

Además del directorio de instalación, `kling` descubre extensiones en este
orden, y la primera que aparece con un nombre gana:

1. los directorios de `$KLING_PLUGIN_PATH` (separados por `:`);
2. `<prefijo>/lib/kindling/plugins/`, junto al binario de `kling` (lo que usan
   los paquetes de sistema);
3. `$XDG_DATA_HOME/kling/plugins/` o `~/.local/share/kling/plugins/`;
4. el `PATH`.

Los ficheros con punto o extensión (`kling-hello.json`) no se consideran, y
`kling-bridge`, `kling-bridge-local` y `kling-guest` nunca se ejecutan como
extensión. `kling` solo busca cuando le hace falta: ante un comando que no es
del núcleo, y en `help`, `completion`, `status`, `config`, `doctor` y
`plugins`; `kling ps` no ejecuta nada de nadie.

Los comandos del núcleo ganan siempre: una extensión que declare `ps` no lo
recibe y `kling plugins ls` lo marca. Entre dos extensiones gana la primera.

`ai`, `chispa` y `models` son **extensiones incorporadas** (`plugin.Builtin`):
viven dentro del binario de `kling`, pero su ayuda, completado y ganchos salen
de un manifiesto como los de cualquier otra, `kling plugins ls` las lista como
`built in` y `kling plugins disable ai` las apaga igual.

## 5. Publicarla para `kling plugins install` (5 minutos)

`kling plugins install <nombre>` baja un ejecutable **verificado por sha256
antes de escribirlo**:

```sh
kling plugins install hello                          # de la release de kindling de este kling
kling plugins install hello@v0.13.0                  # de una release concreta
kling plugins install hello -from https://example.com/releases/v0.1.0/
kling plugins install hello -file ./kling-hello      # un binario local
kling plugins install hello -from https://… -sha256 <hash>
kling plugins install hello -dir /opt/kling/plugins  # otro directorio de instalación
```

Por defecto baja
`https://github.com/juan52878911/kindling/releases/download/<tag>/kling-<nombre>-<os>-<arch>`,
con `<tag>` la versión del propio `kling` (un `kling` de desarrollo exige
`@vX`, `-from` o `-file`), y comprueba el hash contra el `SHA256SUMS` de la
misma release. Después ejecuta `--kling-manifest` y rechaza el binario si no
valida; baja los `companions` del mismo sitio con el mismo control; y deja al
lado `kling-<nombre>.json` con `{name, version, url, sha256, installed}`, que es
lo que `kling plugins ls` enseña como origen.

Para publicar una extensión propia fuera de este repo basta con reproducir esa
forma en cualquier servidor HTTPS:

```sh
V=v0.1.0; mkdir -p dist
for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  GOOS=${p%/*} GOARCH=${p#*/} CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w" -o dist/kling-hello-${p%/*}-${p#*/} .
done
(cd dist && shasum -a 256 kling-* > SHA256SUMS)
gh release create $V dist/*          # o súbelo a donde quieras
```

y quien la use:

```sh
kling plugins install hello -from https://github.com/<tú>/kling-hola/releases/download/v0.1.0/
```

`-from` solo acepta `https://` y exige un `SHA256SUMS` junto al asset o
`-sha256`. Si la extensión trae companions, publícalos en la misma release con
el mismo patrón de nombre (`<companion>-<os>-<arch>`).

Las extensiones del repo (`mcp`, `sandbox`) salen en cada release de
kindling con la misma versión que el núcleo, así que todos los binarios de una
release son compatibles entre sí ([`releases.md`](releases.md)).

Para quitarla: `kling plugins rm hello` borra el binario, su `.json` y sus
companions del directorio de extensiones. Si está en otro sitio del `PATH`, lo
dice y no toca nada.

No hay marketplace ni permisos por extensión: una extensión corre como el
usuario que la ejecuta, y quien habla con el socket del daemon tiene lo mismo
que root en ese host. La garantía es el sha256, un origen que eliges tú y
`kling plugins ls` mostrando la ruta y el hash de cada una.

## 6. Lo que el núcleo ofrece a una extensión

Todo por el API del daemon (`pkg/api`), nunca por dentro:

| Pieza | Para qué |
|---|---|
| **Anotaciones de snapshot** — `PUT/DELETE /snapshots/{name}/annotations/{key}` | datos de la extensión que viven con un snapshot (la de MCP guarda ahí su catálogo y su salud) |
| **Store** — `GET/PUT/DELETE /store/{ns}/{key}` | estado global de la extensión en el host del daemon (la de MCP guarda ahí sus servidores externos enlazados) |
| **Constructores de imágenes** — `POST /images` con `builder` y `spec` | construir imágenes a su manera; el constructor es un ejecutable de root en `/usr/local/lib/kindling/builders/<nombre>` |
| **Ficheros en imágenes** — `GET/PUT /images/{name}/files` | leer o poner al día un fichero dentro de una imagen ya construida |
| **Proxy al invitado** — `POST /machines/{ref}/guest` | hablar con el servidor de dentro de una microVM; ruta, cabeceras y tamaño los decide la extensión |
| `pkg/scheduler` | despertar, congelar por inactividad, réplicas, afinidad de sesión, precalentado y cuotas, con ganchos para lo propio |
| `pkg/guest` | el agente de invitado genérico (exec, volúmenes, MMDS, DNS), para embeberlo en el PID 1 de sus imágenes |

`GET /info` devuelve `capabilities`: la extensión comprueba ahí que el daemon
tiene lo que necesita antes de usarlo, en vez de deducirlo de la versión. La
tabla completa de rutas está en [`api.md`](api.md).
