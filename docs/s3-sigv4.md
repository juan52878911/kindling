# S3 y compatibles con el proxy de credenciales (re-firma SigV4)

Las peticiones a S3, R2, GCS en modo S3, MinIO o Garage van firmadas (SigV4, HMAC): no
basta con cambiar un marcador en una cabecera. Una credencial `Kind: "sigv4"` hace que
el proxy (`pkg/credproxy/sigv4.go`) **descarte la firma del invitado y firme de nuevo con
la clave real**. El invitado solo conoce un access key id marcador (`Placeholder`); su
secret key da igual, el proxy no la usa.

## La credencial

| Campo | Qué es |
|---|---|
| `Domain` | host del servidor (`s3.eu-west-1.amazonaws.com`, `<cuenta>.r2.cloudflarestorage.com`…) |
| `Placeholder` | el access key id que ve el invitado (`AWS_ACCESS_KEY_ID`) |
| `AccessKeyID`, `Secret` | el par real; solo viven en el daemon |
| `Region`, `Service` | con qué se firma (`Service` es `s3` por defecto). Son los de la regla: el alcance (scope) que escriba el invitado se ignora |
| `Allow` | igual que en HTTP: `"GET /bucket/**"`… |

No admite `Headers`, `Query`, `Body` ni `Upstream`. Hoy el proxy solo existe a nivel de
paquete: la API, `kling machine credential -type sigv4` y la persistencia del daemon
(`internal/machine`, `pkg/api`) están por enchufar.

## Qué cubre

- **Cuerpo con hash** (`x-amz-content-sha256` en hex) y `UNSIGNED-PAYLOAD`: el cuerpo pasa
  sin tocar y se firma con el mismo hash. Sin la cabecera (otros servicios) el proxy
  calcula el hash, con el cuerpo en memoria (hasta 1 MiB).
- **`aws-chunked` firmado** (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, con o sin `-TRAILER`):
  una firma por trozo encadenada con la anterior, empezando por la de la petición. Se
  reescriben en flujo, un trozo en memoria cada vez (máx. 4 MiB). Las firmas miden lo
  mismo, así que `Content-Length` no cambia.
- **`STREAMING-UNSIGNED-PAYLOAD-TRAILER`** (aws CLI por HTTPS): no hay firma por trozo; el
  cuerpo pasa tal cual.
- **URL prefirmadas** (`X-Amz-Signature` en la query): se respeta la fecha y la caducidad
  del invitado; el proxy no las alarga.
- En una petición con `Authorization`, `x-amz-date` se pone con el reloj del proxy: la
  deriva de la hora de la microVM no rompe la firma.
- SigV4a (`AWS4-ECDSA-P256-SHA256`) no se admite: 400.

La respuesta conserva su `Content-Length` (los clientes de S3 lo exigen en un GET). Si el
redactor tuviera que cambiar algo en el cuerpo (la secret key dentro de un objeto), la
longitud deja de cuadrar y la respuesta se corta.

## Límites

- Cuerpo de la petición: `MaxBody` (10 MiB). Con aws CLI (partes de 8 MiB) o rclone
  (5 MiB) la subida multiparte cabe; un `PutObject` único de más no.
- **Quien manda peticiones por el proxy usa la credencial con lo que `Allow` deje**: igual
  que en HTTP, el proxy no la oculta del uso, solo del texto.
- La clave real solo entra en el HMAC. No sale en cabeceras al invitado, en la
  auditoría ni en los logs (test `TestSigV4LaClaveRealNuncaSale`).

## Verificación

- Unidad: vectores de botocore con hora fija (S3 con hash de cuerpo, sin cuerpo, servicio
  no S3 con doble codificación, URL prefirmada), el ejemplo de trozos de la documentación
  de S3 y un verificador de servidor propio para los casos de extremo a extremo.
- Laboratorio (`go test -tags s3lab ./pkg/credproxy/`, ver `sigv4_lab_test.go`): Garage
  2.0.0 (imagen `dxflrs/garage`, importada con `kling image import` y arrancada como
  microVM) con aws CLI y rclone detrás del proxy: PUT, GET, listado, multiparte, URL
  prefirmada y `aws-chunked` firmado pasan; con el marcador directo al servidor, rechazado.

## Limitación conocida

`STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` (trozos firmados **con** trailer firmado)
pasa contra el verificador de los tests, que sigue la documentación de AWS, pero Garage
2.0.0 lo rechaza («Invalid payload signature»), también con el prefijo que Garage usa
para el trailer. Sin un segundo servidor real no se sabe de qué lado está. Los clientes
habituales no lo usan por HTTP (aws CLI y rclone firman el cuerpo entero).
