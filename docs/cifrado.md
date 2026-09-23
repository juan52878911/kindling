# Cifrado en reposo

Los snapshots de kindling llevan la memoria de cada microVM: lo que el invitado
tuviera en RAM al congelarse, incluidos datos de sesión. Si el disco se pierde o
se copia, eso queda legible.

## Por qué no lo cifra kindling

Firecracker mapea `mem.file` directamente para restaurar en milisegundos.
Cifrar por encima obligaría a descifrar la memoria entera en cada thaw, o a
escribir una capa de bloques propia. La capa correcta es la de debajo: el disco
donde vive `$KLING_ROOT`. Con LUKS (dm-crypt) es transparente para Firecracker y
cuesta lo que cueste el AES del procesador, que en cualquier CPU con AES-NI o
extensiones de ARMv8 es poco frente a la latencia del disco.

## Cómo saber si lo está

```sh
kling info
```

La línea `at rest:` dice `encrypted (dm-crypt)` si `$KLING_ROOT` está sobre un
dispositivo cifrado, siguiendo la pila hacia abajo (LVM sobre LUKS, por ejemplo),
o avisa de que no lo está. Si no aparece, el daemon no puede saberlo: un sistema
de ficheros sin dispositivo de bloques, u otro sistema operativo.

## Receta: un volumen LUKS para `$KLING_ROOT`

En un disco o partición dedicado (`/dev/sdb` en el ejemplo). **Borra lo que
haya en él.**

```sh
sudo systemctl stop kling-gateway kling
sudo cryptsetup luksFormat /dev/sdb
sudo cryptsetup open /dev/sdb kindling
sudo mkfs.ext4 /dev/mapper/kindling
sudo mount /dev/mapper/kindling /mnt
sudo rsync -a /var/lib/kindling/ /mnt/
sudo umount /mnt
sudo mount /dev/mapper/kindling /var/lib/kindling
sudo systemctl start kling kling-gateway
kling info | grep 'at rest'
```

Para que se abra al arrancar hay que añadirlo a `/etc/crypttab` y `/etc/fstab`,
con la clave en un TPM (`systemd-cryptenroll --tpm2-device=auto`) o introducida a
mano. Un host que arranca sin nadie delante necesita la primera opción.

Los snapshots dorados siguen atados al host (ver el TSC en `docs/hallazgos.md`), así
que migrar `$KLING_ROOT` a un disco cifrado del MISMO host no los invalida.
