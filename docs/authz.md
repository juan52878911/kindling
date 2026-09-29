# Autorización en el socket del daemon

Sin política, **quien alcanza el socket del daemon manda sobre todo**: arranca,
borra y entra en cualquier máquina, lee cualquier snapshot y cambia cualquier
etiqueta. En un host de un solo usuario es lo correcto, y sigue siendo el
comportamiento por defecto. Para compartir un daemon entre varias personas o
equipos, el daemon puede **autorizar cada operación según quién llama**.

## Quién llama

El daemon solo escucha en un socket Unix, así que no necesita credenciales
propias para saber quién está al otro lado: se lo dice el kernel.

- **Linux**: `SO_PEERCRED` da el uid y el gid efectivos del proceso que abrió la
  conexión. Los grupos suplementarios se resuelven por la base de usuarios
  (`/etc/group`) cuando una regla de grupo los necesita.
- **macOS**: `LOCAL_PEERCRED` da el uid efectivo y sus grupos (hasta 16; un
  usuario con más puede no casar con una regla de grupo; las de usuario no
  tienen ese límite).
- **Por SSH** (`kling -H ssh://...`): ssh lanza `kling dial-stdio` como el
  usuario remoto y es ese proceso el que conecta al socket, así que el daemon ve
  al mismo usuario que se autenticó en SSH. No hay nada que configurar aparte.

Se lee **una vez por conexión**, al aceptarla, y no se puede falsificar desde el
cliente: no viaja en la petición.

## La política

Se lee de `/etc/kling/authz.json` al arrancar el daemon, o del fichero que diga
`kling daemon -authz <ruta>` (o `KLING_AUTHZ`). Cambiarla pide reiniciar el daemon.

```json
{
  "rules": [
    {"group": "kling-admin", "role": "admin"},
    {"user": "ana", "role": "tenant:ana"},
    {"uid": 1002, "role": "tenant:equipo-b"},
    {"group": "equipo-c", "role": "tenant:equipo-c"}
  ],
  "shared_templates": ["python", "node"],
  "tokens": [
    {"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "role": "tenant:ci"}
  ]
}
```

- **`rules`**: cada una con exactamente uno de `uid`, `user`, `gid` o `group`, y
  un `role`. Gana la **primera** que casa, en el orden del fichero. Los nombres
  (`user`, `group`) se resuelven al arrancar: uno que no existe es un error.
- **`role`**: `admin` o `tenant:<nombre>` (minúsculas, dígitos, `.`, `_`, `-`).
- **`shared_templates`**: snapshots **sin dueño** (los de un admin) que todos los
  inquilinos pueden ver y usar como plantilla, sin cambiarlos ni borrarlos. Uno
  con dueño no se comparte aunque esté en la lista: un inquilino no puede
  plantar una plantilla para los demás.
- **`tokens`**: opcionales. El `sha256` (hex) de un token y el inquilino que da.
  El cliente lo manda con `KLING_AUTHZ_TOKEN=<token>` (cabecera
  `Authorization: Bearer`), y entonces **el token manda** sobre el usuario del
  sistema: un admin con el token de un inquilino actúa como ese inquilino (así
  un servicio que habla con el daemon en nombre de otros puede ceñirse a cada
  uno). Solo dan roles de inquilino: admin se es por usuario del sistema, nunca
  por un secreto. Un token que no está es un `401`. Se comparan todos, en tiempo
  constante, como los del frontal de `ext/sandbox`. `printf %s "$TOKEN" |
  sha256sum` da el hash.

Todo lo que no se entiende es un error y el daemon no arranca: un campo mal
escrito que se ignorara dejaría a alguien sin el límite que se le quería poner.
Por lo mismo, el fichero tiene que ser **regular** (no un enlace), de **root o del
usuario del daemon** y **no escribible por grupo ni otros**: quien pudiera
reescribirlo se daría admin.

**Sin fichero** en la ruta por defecto, no hay política: todo como siempre, con un
aviso en `kling doctor` y en el log del daemon. Una ruta **pedida** (`-authz`,
`KLING_AUTHZ`) que no existe es un error: el daemon no arranca abierto creyendo
que está cerrado.

**root y el usuario del daemon son siempre admin**: cualquiera de los dos puede
reescribir la política o sustituir el daemon, así que negarles algo no
protegería nada.

Para que un usuario llegue siquiera al socket tiene que poder abrirlo (`0660`,
del usuario y grupo a los que se cede con `-socket-user`): se le añade a ese
grupo. Llegar al socket ya no basta para mandar; sin regla, todo es `403`.

## Qué puede cada rol

| Acción | Rutas | admin | tenant:X | sin rol |
|---|---|---|---|---|
| `info` | `GET /info` | todo | sus máquinas en la cuenta | contesta solo versión, capacidades y su `authz` (ni raíz, ni carpetas, ni almacén) |
| `list` | `GET /machines`, `/sandboxes`, `/snapshots`, `/graphs`, `/events` | todo | solo lo suyo (y las plantillas compartidas) | 403 |
| `create` | `POST /machines`, `/sandboxes`, `/graphs` | todo | sí, con su dueño sellado (abajo) | 403 |
| `machine` | todo `/machines/{ref}/...` y `/sandboxes/{ref}/...` | todo | solo las suyas; las demás, 404 | 403 |
| `snapshot.read` | `GET /snapshots/{name}` | todo | suyos y compartidos | 403 |
| `snapshot.write` | anotaciones, credenciales de plantilla, `DELETE /snapshots/{name}` | todo | solo suyos; una compartida, 403 | 403 |
| `graph` | todo `/graphs/{ref}/...` | todo | solo los suyos | 403 |
| `images.list` | `GET /images` | todo | sí (las imágenes base son de todos) | 403 |
| `admin` | construir y borrar imágenes, sus ficheros y blobs, volúmenes, `/store`, `/shares/uploads`, `/metrics`, `/procstats` | todo | 403 | 403 |

Lo ajeno responde como lo que no existe (`404`): un inquilino no puede sondear
nombres de otros por sus códigos de error.

La tabla vive en el código en un único sitio (`rutas()` en
`internal/daemon/server.go`): cada ruta declara su acción y un único middleware
decide antes de llamar al handler. Una ruta nueva no se puede registrar sin
acción, y el test `TestAuthzAccionesPorRol` recorre la tabla entera con cada rol.

## El dueño: `kling.owner`

Con política, lo que un inquilino crea lleva la etiqueta `kling.owner=<inquilino>`,
y **la pone el daemon**:

- `run`, `sandbox` y cada nodo de `graph up` la reciben aunque no se pida; pedirla
  (o cambiarla con `PUT /machines/{ref}/labels`, o en las etiquetas de un `fork`)
  es un `403`.
- Se hereda como cualquier etiqueta: el `commit` de una máquina suya da un snapshot
  suyo, un `run -from` de ese snapshot una máquina suya, y `fork` y `graph
  snapshot`/`fork` conservan el dueño. Un `run -from` de una plantilla compartida
  da una máquina del inquilino.
- Un grafo es de un inquilino si **todos** sus nodos llevan su `kling.owner`.
- `commit -replace` no pisa el snapshot de otro ni una plantilla compartida.

Un admin sí puede poner `kling.owner` (crear algo a nombre de un inquilino, o
reasignarlo), y su cuerpo no se toca.

### `kling db` y el proxy de credenciales

`kling.db.owner` lo elige quien crea la copia (`kling db -owner`, `local` por
defecto). Con política queda **ligado al inquilino**: si un inquilino lo pone,
tiene que ser su nombre (`kling db ... -owner <inquilino>`), y una instancia de una
plantilla compartida que traiga el `kling.db.owner` de otro recibe el del inquilino.
Una credencial con `upstream_machine` solo puede llevar a una máquina del mismo
inquilino, con `upstream_owner` igual a él; la regla vale igual para las credenciales
de una máquina (`POST /machines/{ref}/credentials`) y las de una plantilla
(`PUT /snapshots/{name}/credentials`).

Además, el proxy de Postgres, que en cada conexión exige que copia, agente y
credencial digan el mismo `kling.db.owner`, exige ahora también el mismo
`kling.owner` en copia y agente. Sin política los dos están vacíos y no cambia
nada; con ella, ni un admin que ponga el mismo `kling.db.owner` a máquinas de dos
inquilinos cruza la frontera. Cambiar `kling.owner` de una máquina corta sus
sesiones como cambiar sus etiquetas de `kling db`.

## Lo que un inquilino no puede usar (todavía)

- **Volúmenes** (`-volume`, `volumes`): no tienen dueño, y un volumen compartido
  entre inquilinos sería un canal entre ellos. `403` en `run`, `sandbox` y los nodos
  de un grafo, y todas las rutas `/volumes` son de admin. (Una plantilla compartida
  de un admin que traiga volúmenes los reengancha: es decisión del admin al
  compartirla.)
- **Carpetas del host** (`-share`): son del host. `403`.
- **El store** (`/store`), las **métricas del host** y **construir imágenes**:
  de admin.

## Comprobarlo

```
$ kling info
...
authz:        policy on; you are tenant:ana
$ kling doctor
...
✓ authz       policy on; you are tenant:ana
```

Sin política:

```
! authz       no authz policy: whoever reaches the daemon's socket controls everything
              fix: fine on a single-user host; to share it, write /etc/kling/authz.json and restart the daemon (docs/authz.md)
```

Sin rol:

```
$ kling ps
error: uid 1003 has no role in the daemon's authz policy (/etc/kling/authz.json)
```

## Límites de este MVP

- **Los nombres son globales.** Dos inquilinos no pueden tener una máquina, un
  snapshot o un grafo con el mismo nombre, y el `409` de un nombre ocupado dice que
  existe. No dice de quién ni deja tocarlo.
- **La política se lee al arrancar.** Cambiarla pide reiniciar el daemon.
- **Sin cuotas.** Un inquilino puede llenar el host de máquinas hasta los topes del
  daemon (`KLING_MAX_MACHINES`, memoria, disco). El reparto por inquilino con cuotas
  lo hace el frontal de `ext/sandbox`.
- **La frontera es el daemon, no el invitado.** Dos inquilinos siguen compartiendo
  host, kernel y KVM; lo que separa sus microVMs es lo de siempre (`SECURITY.md`).
- **`GET /events`** filtra por las máquinas vivas del inquilino: el evento de una
  máquina ya borrada no se puede atribuir y no se envía.
