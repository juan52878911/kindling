# Archivar kindling-mcp y kindling-sandbox

Desde kindling v0.13.0 las dos extensiones viven en este repositorio:
`ext/mcp` (antes [kindling-mcp](https://github.com/juan52878911/kindling-mcp),
última release v0.4.0) y `ext/sandbox` (antes
[kindling-sandbox](https://github.com/juan52878911/kindling-sandbox), última
release v0.2.2). Se importaron con su historial: `git log -- ext/mcp` enseña los
commits originales con las rutas reescritas. Las etiquetas viejas no se trajeron;
sus releases siguen en los repos de origen.

Este documento dice qué queda en los repos viejos y cómo archivarlos. **Los
pasos se ejecutan a mano, una vez publicada kindling v0.13.0**: antes, el enlace
a la release nueva no existe y quien llegue a los repos viejos no tendría adónde
ir.

## El README final

Un único commit en `main` de cada repo viejo sustituye el README (y su
traducción, si la tiene) por este texto. Para kindling-mcp, con `<x>` = `mcp`;
para kindling-sandbox, con `<x>` = `sandbox`:

```markdown
# kindling-<x>

**Movido a `kindling/ext/<x>` desde kindling v0.13.0; las releases anteriores siguen aquí.**

- Código, issues y releases nuevas: https://github.com/juan52878911/kindling/tree/main/ext/<x>
- Instalación: `kling plugins install <x>` (o `install.sh --with <x>` de kindling).
- Novedades: https://github.com/juan52878911/kindling/blob/main/CHANGELOG.md

Todos los binarios de una release de kindling son compatibles entre sí: ya no hay
tabla de versiones que casar entre este repo y el núcleo.

Este repositorio está archivado (solo lectura). No se borra: las releases hasta
la última publicada aquí siguen descargables y sus etiquetas no se mueven.
```

## El `install.sh` viejo

Hay instrucciones por ahí con `curl …/kindling-<x>/main/scripts/install.sh | sh`.
En el mismo commit, `scripts/install.sh` del repo viejo pasa a delegar en el de
kindling, conservando los argumentos:

```sh
#!/bin/sh
# kindling-<x> se mudó a kindling/ext/<x> en kindling v0.13.0: este instalador
# solo delega en el de kindling, que instala el núcleo y la extensión juntos.
set -eu
echo "kindling-<x> moved to kindling/ext/<x> (kindling v0.13.0); using kindling's installer" >&2
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh \
  | sh -s -- --with <x> "$@"
```

Quien pida una versión vieja con `--tag v0.4.0` recibirá un error del instalador
nuevo, porque esa etiqueta no existe en kindling; las releases viejas se bajan a
mano desde el repo archivado.

## Pasos para archivar (no ejecutados)

Con `gh` autenticado como dueño de los repos, y para cada `<x>` en `mcp` y
`sandbox`:

```sh
# 0. kindling v0.13.0 publicada y con los assets kling-<x>-<os>-<arch>
gh release view v0.13.0 -R juan52878911/kindling --json assets -q '.assets[].name' | grep "kling-<x>-"

# 1. clon temporal del repo viejo
T=$(mktemp -d)
gh repo clone juan52878911/kindling-<x> "$T/kindling-<x>"
cd "$T/kindling-<x>"

# 2. el README y el install.sh de arriba
$EDITOR README.md README.es.md scripts/install.sh   # README.es.md solo en mcp
git commit -am "readme: movido a kindling/ext/<x>"
git push origin main

# 3. descripción del repo con el nuevo destino
gh repo edit juan52878911/kindling-<x> \
  --description "Moved to kindling/ext/<x> (kindling v0.13.0). Archived." \
  --homepage https://github.com/juan52878911/kindling/tree/main/ext/<x>

# 4. cerrar o trasladar lo abierto
gh issue list -R juan52878911/kindling-<x> --state open
gh pr list    -R juan52878911/kindling-<x> --state open
#    cada issue vivo: gh issue transfer <n> juan52878911/kindling

# 5. archivar: solo lectura. NO borrar el repo ni sus releases o etiquetas.
gh repo archive juan52878911/kindling-<x> --yes
```

Comprobaciones después:

- `gh repo view juan52878911/kindling-<x> --json isArchived` dice `true`.
- `https://github.com/juan52878911/kindling-<x>/releases` sigue listando las
  releases viejas.
- `curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling-<x>/main/scripts/install.sh | sh -s -- --dry-run`
  delega en el instalador de kindling.
- La imagen `ghcr.io/juan52878911/kindling-operator` sigue recibiendo etiquetas
  desde el workflow de kindling: el paquete de GHCR va por nombre, no por repo.
  Si el paquete estaba vinculado a kindling-sandbox, en su página de ajustes se
  vincula a kindling para que el workflow nuevo pueda empujar
  (*Manage Actions access* → añadir `juan52878911/kindling` con rol *Write*).
