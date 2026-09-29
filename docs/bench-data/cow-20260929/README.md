# Copia al escribir de los discos de instancia — 2026-09-29

Host: CT 105 del laboratorio (Proxmox LXC privilegiado, kernel 6.17.2-1-pve, raíz en
**ext4**, sin módulo XFS en el kernel). `daemon.cow=auto` eligió el **almacén Btrfs** de
kindling (`$root/cow.btrfs`, 1,79 GiB reservados enteros, montado por loop con
`nodev,nosuid,noexec` y sin `discard`). Golden `pg` (Postgres 16 caliente, 83 MiB de
overlay). `scripts/bench-cow.sh`, N instancias seguidas con `run -from`.

| modo | real | N | primera ms | media ms | p50 ms | máx ms | disco añadido MiB |
|---|---|---:|---:|---:|---:|---:|---:|
| off | copia completa | 1 | 83 | 83 | 83 | 83 | 83 |
| off | copia completa | 8 | 88 | 90 | 85 | 126 | 664 |
| off | copia completa | 32 | 84 | 113 | 86 | 456 | 2 658 |
| auto | almacén Btrfs | 1 | 57 | 57 | 57 | 57 | 3 |
| auto | almacén Btrfs | 8 | 57 | 58 | 58 | 60 | 28 |
| auto | almacén Btrfs | 32 | 57 | 85 | 100 | 113 | 115 |

- Disco: 23× menos con 32 instancias (cada una guarda solo lo que escribe).
- Latencia máxima con 32: 456 → 113 ms (4×). Por instancia, ~30 % menos; la diferencia
  crece con el tamaño del overlay del golden (con 200 MiB la copia costaba 76 ms sola).
- `kling info`/`kling doctor`: `store (reflink inside kindling's Btrfs store)`.
- Tests con montajes reales en este CT (`KLING_TEST_MOUNTS=1`): `TestAlmacenBtrfsDeVerdad`,
  `TestBindDelAlmacenEnElJail`, `TestCopiarDisperso*` — PASS.
