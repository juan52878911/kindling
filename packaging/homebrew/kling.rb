# Fórmula de Homebrew de kindling. Es una PLANTILLA: release.yml la rellena
# con packaging/homebrew/formula.sh (versión y sha256 de cada binario, sacados
# del SHA256SUMS de la release) y la publica como asset kling.rb. Instalar:
#
#   brew install --formula ./kling.rb      # el asset de la release
#
# brew comprueba cada binario solo por su sha256. kling.rb está en el
# SHA256SUMS firmado de la release: quien quiera más, comprueba la firma y la
# suma de kling.rb antes de instalarla (docs/firmas.md).
#
# formula.sh quita esta cabecera (hasta la línea de «van firmados»); el bloque
# que la sigue sí se publica, en el asset y en el tap juan52878911/kindling.
#
# Los binarios son los de la release, no se compila nada: kling-vz y kling-hv
# van firmados con su entitlement y una recompilación lo perdería.
# kindling @VERSION@: binarios publicados en
# https://github.com/juan52878911/kindling/releases/tag/v@VERSION@
# Cada url se comprueba por su sha256, copiado del SHA256SUMS de la release.
# Desde v1.0.0 ese SHA256SUMS va firmado con minisign; para comprobarlo:
#   gh release download v@VERSION@ --repo juan52878911/kindling \
#     -p SHA256SUMS -p SHA256SUMS.minisig
#   minisign -V -P RWQ9r2LB7j2GE1A0GedCeER+ZFHmX4E1NPubT39GBQDho/awRnY6WpVN \
#     -m SHA256SUMS -x SHA256SUMS.minisig   # "tag:v@VERSION@" en el comentario
# y que los sha256 de esta fórmula sean los de ese SHA256SUMS (docs/firmas.md).
# Es una fórmula y no un cask: brew no toca los binarios (no se recolocan ni se
# vuelven a firmar, así kling-vz/kling-hv conservan su entitlement), no les pone
# cuarentena, trae `brew services` y vale también para Linux.
class Kling < Formula
  desc "Firecracker microVMs (Virtualization.framework on macOS) for agent tools"
  homepage "https://github.com/juan52878911/kindling"
  license "Apache-2.0"

  # Los Mac Intel no tienen backend (ni kling-vz ni kling-hv); en Linux valen
  # amd64 y arm64.
  depends_on arch: :arm64 if OS.mac?
  # mkfs.ext4 y debugfs: el overlay de cada microVM y la inspección de imágenes.
  depends_on "e2fsprogs"

  on_macos do
    on_arm do
      url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-darwin-arm64"
      sha256 "@SHA256_kling-darwin-arm64@"

      resource "kling-vz" do
        url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-vz-darwin-arm64"
        sha256 "@SHA256_kling-vz-darwin-arm64@"
      end

      resource "kling-hv" do
        url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-hv-darwin-arm64"
        sha256 "@SHA256_kling-hv-darwin-arm64@"
      end
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-linux-amd64"
      sha256 "@SHA256_kling-linux-amd64@"

      resource "kling-guest" do
        url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-guest-linux-amd64"
        sha256 "@SHA256_kling-guest-linux-amd64@"
      end
    end

    on_arm do
      url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-linux-arm64"
      sha256 "@SHA256_kling-linux-arm64@"

      resource "kling-guest" do
        url "https://github.com/juan52878911/kindling/releases/download/v@VERSION@/kling-guest-linux-arm64"
        sha256 "@SHA256_kling-guest-linux-arm64@"
      end
    end
  end

  def install
    bin.install Dir["kling-*"].first => "kling"
    if OS.mac?
      # Al lado de kling, que es donde el daemon busca su VMM.
      resource("kling-vz").stage { bin.install Dir["kling-vz-*"].first => "kling-vz" }
      resource("kling-hv").stage { bin.install Dir["kling-hv-*"].first => "kling-hv" }
    else
      # El agente que el daemon mete en las imágenes de Docker (KLING_LIB_DIR).
      resource("kling-guest").stage { (libexec/"kindling").install Dir["kling-guest-*"].first => "kling-guest" }
    end
  end

  def caveats
    s = <<~EOS
      Diagnose this host and start the daemon:
        kling up
      Upgrade with `brew upgrade kling` (not `kling upgrade`).
    EOS
    if OS.linux?
      s += <<~EOS

        On Linux the daemon runs as root and needs KVM and Firecracker
        (`kling up` says how to install it). The service sets
        KLING_LIB_DIR=#{opt_libexec}/kindling for the guest agent.
      EOS
    end
    s
  end

  # Lo mismo que `kling up -install` en macOS: `kling daemon` siempre vivo, con
  # su registro aparte. En Linux, como root (el kling.service de systemd).
  service do
    run [opt_bin/"kling", "daemon"]
    environment_variables KLING_LIB_DIR: opt_libexec/"kindling" if OS.linux?
    require_root true if OS.linux?
    keep_alive true
    log_path var/"log/kling.log"
    error_log_path var/"log/kling.log"
  end

  test do
    # Sin daemon: la versión y los formatos de estado que lee este binario.
    assert_match "\"kling\":\"#{version}\"", shell_output("#{bin}/kling upgrade -schemas")
  end
end
