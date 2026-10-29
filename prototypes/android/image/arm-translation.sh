#!/usr/bin/env bash
# arm-translation.sh — la traducción ARM de una imagen amd64 (issue #93,
# docs/traduccion-arm.md) sobre un rootfs de Redroid ya extraído. Lo llama
# build-image.sh con ARM_TRANSLATION; es lo mismo que hace el constructor Go
# (internal/android/translation.go y libndk.go):
#
#   arm-translation.sh ROOTFS none    CACHE   quita el puente que trae Redroid
#                                             (por defecto en amd64): ABIs x86_64
#   arm-translation.sh ROOTFS libndk  CACHE   quita el de Redroid y pone el
#                                             libndk_translation de la imagen
#                                             del emulador de Google (fijada)
#   arm-translation.sh ROOTFS redroid CACHE   no toca nada
#
# CACHE es donde queda el zip del emulador (1,4 GiB, se comprueba su sha256).
# Necesita python3 y debugfs (e2fsprogs), que build-image.sh ya pide.
set -euo pipefail

ROOTFS="$1"; MODE="$2"; CACHE="$3"
# Lo mismo que libndkPin (internal/android/pins.go): la imagen de sistema
# x86_64 de Android 14 (API 34, Google APIs, r14). Tamaño y sha1 de la lista
# oficial sys-img2-3.xml; sha256 calculado sobre el fichero de esa URL.
NDK_URL="https://dl.google.com/android/repository/sys-img/google_apis/x86_64-34_r14.zip"
NDK_SHA256="783a40134baf4f3012d4464fbe1571b1612a0dbd2e7a44d14bd8328923443833"
NDK_SIZE=1563721130
NDK_VERSION=0.2.3
RUNNER=/system/bin/ndk_translation_program_runner_binfmt_misc_arm64
WRAPPER=/system/bin/kindling-ndk-binfmt

log() { printf '==> %s\n' "$*"; }
die() { printf 'arm-translation.sh: %s\n' "$*" >&2; exit 1; }
[ -d "$ROOTFS/system" ] || die "$ROOTFS is not a Redroid rootfs"
case "$MODE" in none|libndk|redroid) ;; *) die "mode must be none, libndk or redroid" ;; esac
[ "$MODE" = redroid ] && { log "arm translation: redroid (left as shipped)"; exit 0; }

# ── quitar el puente de Redroid (none y libndk) ─────────────────────────────
n=0
for p in "$ROOTFS"/system/lib64/libndk_translation*.so "$ROOTFS"/system/lib/libndk_translation*.so \
         "$ROOTFS"/system/lib64/libnb.so "$ROOTFS"/system/lib/libnb.so \
         "$ROOTFS"/system/lib64/arm64 "$ROOTFS"/system/lib/arm "$ROOTFS"/system/bin/arm64 "$ROOTFS"/system/bin/arm \
         "$ROOTFS"/system/bin/ndk_translation_program_runner_binfmt_misc "$ROOTFS$RUNNER" \
         "$ROOTFS"/system/etc/binfmt_misc "$ROOTFS"/system/etc/init/ndk_translation.rc \
         "$ROOTFS"/system/etc/ld.config.arm64.txt "$ROOTFS"/system/etc/ld.config.arm.txt; do
  if [ -e "$p" ] || [ -L "$p" ]; then rm -rf "$p"; n=$((n + 1)); fi
done
log "arm translation: removed $n native bridge paths shipped by Redroid"

# ── poner el de Google (libndk) ─────────────────────────────────────────────
if [ "$MODE" = libndk ]; then
  command -v debugfs >/dev/null || die "libndk needs debugfs (e2fsprogs)"
  mkdir -p "$CACHE"
  zip="$CACHE/$(basename "$NDK_URL")"
  if ! [ -f "$zip" ] || [ "$(sha256sum "$zip" | cut -d' ' -f1)" != "$NDK_SHA256" ]; then
    log "arm translation: downloading $NDK_URL ($((NDK_SIZE >> 20)) MiB)"
    curl -fsSL --retry 3 -o "$zip.part" "$NDK_URL"
    [ "$(stat -c %s "$zip.part")" = "$NDK_SIZE" ] || die "$zip: wrong size"
    [ "$(sha256sum "$zip.part" | cut -d' ' -f1)" = "$NDK_SHA256" ] || die "$zip: sha256 mismatch"
    mv "$zip.part" "$zip"
  fi
  tmp="$(mktemp -d "$CACHE/.libndk.XXXXXX")"
  trap 'rm -rf "$tmp"' EXIT
  # La partición dinámica "system" del super del system.img, dispersa, sin
  # escribir los 4 GiB del disco (el mismo algoritmo que libndk.go).
  python3 - "$zip" "$tmp/system.ext4" <<'PY'
import struct, sys, zipfile
zp, out = sys.argv[1], sys.argv[2]
S = 512
src = zipfile.ZipFile(zp).open("x86_64/system.img")
buf = b""
def need(n):
    global buf
    while len(buf) < n:
        c = src.read(min(1 << 20, n - len(buf)))
        if not c: sys.exit("system.img too short")
        buf += c
need(2 * S)
assert buf[S:S + 8] == b"EFI PART", "not a GPT disk"
lba, num, esz = struct.unpack_from("<QII", buf, S + 72)
need(lba * S + min(num, 256) * esz)
superoff = None
for i in range(min(num, 256)):
    e = buf[lba * S + i * esz: lba * S + (i + 1) * esz]
    if e[56:128].decode("utf-16le").rstrip("\0") == "super":
        superoff = struct.unpack_from("<Q", e, 32)[0] * S
assert superoff is not None, "no super partition"
need(superoff + 4096 + 52)
assert struct.unpack_from("<I", buf, superoff + 4096)[0] == 0x616c4467, "no liblp geometry"
h = superoff + 4096 + 2 * 4096
need(h + 128)
magic, major, _, hsz = struct.unpack_from("<IHHI", buf, h)
assert magic == 0x414c5030 and major == 10, "bad liblp header"
tsz = struct.unpack_from("<I", buf, h + 44)[0]
need(h + hsz + tsz)
t = buf[h + hsz: h + hsz + tsz]
d = [struct.unpack_from("<III", buf, h + 80 + 12 * i) for i in range(2)]
ext = [struct.unpack_from("<QIQI", t, d[1][0] + i * d[1][2]) for i in range(d[1][1])]
runs, size = [], 0
for i in range(d[0][1]):
    e = t[d[0][0] + i * d[0][2]:]
    if e[:36].rstrip(b"\0") != b"system": continue
    first, n = struct.unpack_from("<II", e, 40)
    for ns, typ, data, dev in ext[first:first + n]:
        assert (typ == 0 and dev == 0) or typ == 1, "unsupported extent"
        if typ == 0: runs.append((superoff + data * S, size, ns * S))
        size += ns * S
assert size, "no system partition"
runs.sort()
pos = len(buf)
with open(out, "wb") as f:
    for img, logical, n in runs:
        done = 0
        if img < len(buf):
            k = min(n, len(buf) - img)
            f.seek(logical); f.write(buf[img:img + k]); done = k
        while pos < img + done:
            pos += len(src.read(min(1 << 20, img + done - pos)))
        while done < n:
            c = src.read(min(1 << 20, n - done))
            if not c: sys.exit("system.img too short")
            for o in range(0, len(c), 4096):
                blk = c[o:o + 4096]
                if blk.count(0) != len(blk):
                    f.seek(logical + done + o); f.write(blk)
            done += len(c); pos += len(c)
    f.truncate(size)
PY
  img="$tmp/system.ext4"
  dbg() { debugfs -R "$1" "$img" 2>/dev/null; }
  mkdir -p "$ROOTFS/system/etc/binfmt_misc"
  for d in lib64 bin; do dbg "rdump /system/$d/arm64 $ROOTFS/system/$d"; done
  for f in $(dbg "ls -p /system/lib64" | awk -F/ '$6 ~ /^libndk_translation.*\.so$/ { print $6 }'); do
    dbg "dump -p /system/lib64/$f $ROOTFS/system/lib64/$f"
  done
  for f in "$RUNNER" /system/etc/binfmt_misc/arm64_exe /system/etc/binfmt_misc/arm64_dyn \
           /system/etc/init/ndk_translation.rc /system/etc/ld.config.arm64.txt; do
    dbg "dump -p $f $ROOTFS$f"
  done
  for f in /system/lib64/libndk_translation.so /system/lib64/arm64/libc.so /system/bin/arm64/linker64 "$RUNNER" \
           /system/etc/binfmt_misc/arm64_exe /system/etc/init/ndk_translation.rc; do
    [ -s "$ROOTFS$f" ] || die "no $f in the emulator image"
  done
  # El intérprete que arregla argv[0] (ver translation.go, binfmtWrapperSh).
  cat >"$ROOTFS$WRAPPER" <<EOF
#!/system/bin/sh
# Generado por kindling (arm_translation libndk; docs/traduccion-arm.md).
# binfmt_misc (bandera P) llama: <esto> <ruta del programa> <argv[0]> <args...>
f="\$1"; a0="\$2"; shift 2
case "\$a0" in */*) ;; *) a0="\$f" ;; esac
exec $RUNNER "\$f" "\$a0" "\$@"
EOF
  chmod 0755 "$ROOTFS$WRAPPER"; chown 0:2000 "$ROOTFS$WRAPPER"
  sed -i "s#:$RUNNER:#:$WRAPPER:#" "$ROOTFS"/system/etc/binfmt_misc/arm64_exe "$ROOTFS"/system/etc/binfmt_misc/arm64_dyn
  log "arm translation: libndk_translation $NDK_VERSION from the Android 14 (API 34) emulator image"
fi

# ── propiedades (como editProps de translation.go) ──────────────────────────
python3 - "$ROOTFS" "$MODE" "$NDK_VERSION" <<'PY'
import os, re, sys
root, mode, ver = sys.argv[1:]
bridge = ["ro.dalvik.vm.native.bridge", "ro.enable.native.bridge.exec", "ro.vendor.enable.native.bridge.exec",
          "ro.vendor.enable.native.bridge.exec64", "ro.dalvik.vm.isa.arm", "ro.dalvik.vm.isa.arm64",
          "ro.ndk_translation.version", "ro.ndk_translation.flags"]
st = {k: "" for k in bridge}
if mode == "none":
    st.update({"ro.dalvik.vm.native.bridge": "0", "ro.enable.native.bridge.exec": "0"})
else:
    st.update({"ro.dalvik.vm.native.bridge": "libndk_translation.so", "ro.enable.native.bridge.exec": "1",
               "ro.vendor.enable.native.bridge.exec": "1", "ro.vendor.enable.native.bridge.exec64": "1",
               "ro.dalvik.vm.isa.arm64": "x86_64", "ro.ndk_translation.version": ver,
               "ro.ndk_translation.flags": "accurate-sigsegv"})
abi = re.compile(r"^ro\.(?:system\.|vendor\.|odm\.|product\.)?product\.cpu\.abilist(32|64)?$")
arm = {"arm64-v8a", "armeabi-v7a", "armeabi"}
def fix(v, bits):
    out = [a.strip() for a in v.split(",") if a.strip() and a.strip() not in arm]
    if mode == "libndk" and bits != "32": out.append("arm64-v8a")
    return ",".join(out)
found, seen = set(), set()
files = ["system/build.prop", "system_ext/etc/build.prop", "system/system_ext/etc/build.prop", "vendor/build.prop",
         "odm/etc/build.prop", "vendor/odm/etc/build.prop", "product/etc/build.prop", "system/product/etc/build.prop"]
for rel in files:
    p = os.path.join(root, rel)
    if not os.path.isfile(p) or os.path.realpath(p) in seen: continue
    seen.add(os.path.realpath(p))
    out = []
    for l in open(p).read().splitlines(True):
        body = l.rstrip("\r\n"); k, eq, v = body.partition("=")
        k = k.strip()
        if not eq or k.startswith("#"): out.append(l); continue
        m = abi.match(k)
        if m: out.append(k + "=" + fix(v, m.group(1)) + l[len(body):]); continue
        if k in st:
            found.add(k)
            out.append("# kindling-arm-translation: " + l if st[k] == "" else k + "=" + st[k] + l[len(body):]); continue
        out.append(l)
    txt = "".join(out)
    if rel == "vendor/build.prop":
        add = [k + "=" + st[k] for k in bridge if st[k] and k not in found]
        if add:
            if not txt.endswith("\n"): txt += "\n"
            txt += "# >>> kindling-arm-translation (%s)\n%s\n# <<< kindling-arm-translation\n" % (mode, "\n".join(add))
    open(p, "w").write(txt)
PY
