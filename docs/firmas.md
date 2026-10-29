# Firmas de las releases

Cada release de kindling lleva dos pruebas de origen, además del `SHA256SUMS`:

- **`SHA256SUMS.minisig`**: la firma ed25519 del `SHA256SUMS` en el formato de
  [minisign](https://jedisct1.github.io/minisign/). Como el `SHA256SUMS` lista el
  sha256 de cada asset, firmarlo firma todos. La comprueban `kling upgrade`, la
  descarga de imágenes de `kling up` y `scripts/install.sh` (si hay `minisign`).
- **Atestación de procedencia** (SLSA, Sigstore) de cada asset, hecha por
  `actions/attest-build-provenance` con el OIDC del job de `release.yml`: dice qué
  workflow, commit y etiqueta produjeron cada fichero. No usa ningún secreto. Se
  comprueba con `gh attestation verify`.

La firma de minisign es la que comprueba kling, porque se verifica con la stdlib
(`crypto/ed25519` y una BLAKE2b de 60 líneas en `internal/upgrade/blake2b.go`;
el núcleo no admite dependencias). La atestación es la prueba adicional para quien
tenga `gh`.

## Qué se exige y desde cuándo

| Caso | Antes de v1.0.0 | Desde v1.0.0 |
|---|---|---|
| Release con firma válida para su etiqueta | se acepta | se acepta |
| Firma inválida, de otra clave o de otra etiqueta | **error** | **error** |
| Release sin `SHA256SUMS.minisig` | aviso, solo sha256 | **error** |
| Release firmada y un kling sin claves compiladas | aviso, solo sha256 | **error** |
| `-from-dir` sin firma | aviso | aviso |
| `install.sh` sin `minisign` instalado | aviso, solo sha256 | aviso, solo sha256 |

«Desde v1.0.0» cuenta dos cosas (`upgrade.FirmaObligatoriaDesde`): la versión de
la release que se baja (un `-rc` de la 1.0 ya cuenta) y la del kling que la baja.
Un kling 1.x no instala una release sin firmar, ni siquiera una 0.x; para eso
queda `-from-dir` con los binarios y su `SHA256SUMS`.

La firma lleva en su comentario de confianza `tag:<etiqueta>`, y kling exige que
coincida con la release que pidió: si no, el `SHA256SUMS` firmado de una release
vieja (con sus fallos ya conocidos) valdría para servirla como si fuera otra.

Un fallo al bajar la firma que no sea un 404 (red, 5xx) es un error, no «release
sin firma».

## Generar la clave (una vez, Juan)

```sh
scripts/firma-clave.sh            # escribe ~/.kindling-release-key/kling-release.{key,pub}
```

Usa `go run ./internal/upgrade/firmar genkey` y escribe la clave secreta **sin
cifrar** en el formato de `minisign -G -W` (también vale una hecha así con
minisign; una cifrada con contraseña no, porque scrypt no está en la stdlib). El
script imprime los pasos:

1. **Secreta → GitHub.** En *Settings → Environments* crear `release` con
   revisores obligatorios y solo las etiquetas `v*` como permitidas, y:

   ```sh
   gh secret set KLING_RELEASE_SIGNING_KEY --env release < ~/.kindling-release-key/kling-release.key
   ```

   El job `release` de `release.yml` corre en ese environment; ningún otro job
   ve el secreto.
2. **Secreta → fuera del Mac.** Una copia en el gestor de contraseñas (o un USB
   cifrado) y `rm ~/.kindling-release-key/kling-release.key`.
3. **Pública → código**, en un commit anterior a la primera release firmada:
   la línea `RW…` en `clavesRelease` de `internal/upgrade/firma.go` y en
   `KLING_RELEASE_PUBKEYS` de `scripts/install.sh` (un test exige que coincidan).

Mientras el secreto no exista, el paso «firmar SHA256SUMS» de `release.yml` se
salta con un aviso y la release 0.x sale sin firma, como hasta ahora. Una
release ≥ 1.0 sin secreto falla en ese paso. Con el secreto puesto y la pública
sin añadir al código, el paso también falla: `firmar sign` comprueba la firma
contra las claves compiladas de ese mismo commit antes de publicarla.

## Rotar la clave

Hay varias claves de confianza a la vez para que rotar no rompa a nadie:

1. Generar la nueva (`scripts/firma-clave.sh ~/.kindling-release-key-2`).
2. Release N: añadir la pública nueva a `clavesRelease` e `install.sh` **sin
   quitar la vieja**, y seguir firmando con la vieja.
3. Release N+1: cambiar el secreto `KLING_RELEASE_SIGNING_KEY` por la nueva.
   Los kling ≥ N ya la conocen.
4. Una o dos releases después, quitar la vieja de las dos listas.

Un kling anterior a N no conoce la nueva: desde N+1 tiene que pasar por N
(`kling upgrade -tag vN`) o reinstalar con `install.sh`.

**Si la clave se filtra**: quitarla de las listas y cambiar el secreto en la
misma release, revocar el acceso al environment y avisar en las notas. No hay
lista de revocación: los kling anteriores siguen fiándose de ella hasta que se
actualicen.

## Verificar una release a mano

```sh
TAG=v1.0.0
gh release download "$TAG" -R juan52878911/kindling -p SHA256SUMS -p SHA256SUMS.minisig -p kling-linux-amd64

# 1. La firma, con minisign y la clave pública de clavesRelease:
minisign -V -P RW… -m SHA256SUMS -x SHA256SUMS.minisig
#    "Trusted comment: … tag:v1.0.0 …" tiene que decir la misma etiqueta.

# 2. El asset, contra el SHA256SUMS ya firmado:
sha256sum -c --ignore-missing SHA256SUMS      # macOS: shasum -a 256 -c --ignore-missing

# 3. La procedencia: que salió del workflow de release de este repo.
gh attestation verify kling-linux-amd64 -R juan52878911/kindling \
  --signer-workflow juan52878911/kindling/.github/workflows/release.yml
```

Sin minisign, desde un checkout del repo:

```sh
go run ./internal/upgrade/firmar verify -tag "$TAG" -in SHA256SUMS
```

## Formato

`internal/upgrade/firma.go` lee y escribe el formato de minisign tal cual:
clave pública `base64("Ed" | id[8] | pk[32])`, firma `"ED"` (ed25519 sobre
BLAKE2b-512 del fichero, lo que hace minisign por defecto) o `"Ed"` (sobre el
fichero, el formato antiguo), y la firma global sobre la firma más el comentario
de confianza. El que escribe el paso de release es
`timestamp:<unix>\tfile:SHA256SUMS\ttag:<etiqueta>\thashed`.

Los pares de `internal/upgrade/testdata/firma/` son **solo de test**: un test
comprueba que ninguno está entre las claves de confianza.
