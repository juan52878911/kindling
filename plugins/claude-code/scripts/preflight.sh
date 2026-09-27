#!/bin/sh
# preflight.sh — what /kindling:setup needs to know before asking anything.
# Read-only: it runs nothing that changes the machine and prints one line per
# fact, so the skill (and the person) can see what is already there.

os="$(uname -s 2>/dev/null)"; arch="$(uname -m 2>/dev/null)"
echo "os: $os $arch"
case "$os-$arch" in
    Darwin-arm64) echo "runtime: macOS Apple Silicon -> local daemon possible (backend vz; needs macOS 14+ and e2fsprogs)" ;;
    Darwin-*)     echo "runtime: macOS Intel -> no local daemon (no vz backend); use a remote daemon over ssh://" ;;
    Linux-*)      if [ -e /dev/kvm ]; then echo "runtime: Linux with /dev/kvm -> local daemon possible (Firecracker)"; else echo "runtime: Linux without /dev/kvm -> no local daemon here; use a remote daemon over ssh://"; fi ;;
    *)            echo "runtime: unsupported OS ($os): kling runs on Linux and macOS" ;;
esac
[ "$os" = Darwin ] && { sw_vers -productVersion 2>/dev/null | sed 's/^/macos: /'; }
if [ "$os" = Darwin ]; then
    if command -v brew >/dev/null 2>&1; then echo "brew: $(command -v brew)"; else echo "brew: not found"; fi
    for d in /opt/homebrew/opt/e2fsprogs /usr/local/opt/e2fsprogs; do
        [ -x "$d/sbin/mkfs.ext4" ] && echo "e2fsprogs: $d" && break
    done
    command -v mkfs.ext4 >/dev/null 2>&1 && echo "e2fsprogs: $(command -v mkfs.ext4)"
    [ -f "$HOME/Library/LaunchAgents/dev.kindling.daemon.plist" ] && echo "launchd: dev.kindling.daemon.plist installed" || echo "launchd: no daemon agent installed"
fi

if command -v kling >/dev/null 2>&1; then
    echo "kling: $(command -v kling) $(kling version -json 2>/dev/null | sed -n 's/.*"cli":"\([^"]*\)".*/\1/p')"
    kling version -json 2>/dev/null | sed -n 's/.*"daemon":"\([^"]*\)".*/daemon: \1/p;s/.*"daemon_error":"\([^"]*\)".*/daemon: not responding (\1)/p'
    kling plugin ls 2>/dev/null | awk 'NR>1 && NF && $1 != "An" {print "extension: " $1 " " $2 " " $3} /^$/ {exit}'
    echo "contexts: $(kling context ls 2>/dev/null | tr '\n' ';' | cut -c1-300)"
    kling config show 2>/dev/null | grep -E '^(gateway\.url|daemon\.vmm|contexto|context)' | sed 's/^/config: /'
    if kling config show 2>/dev/null | grep -Eq '^gateway\.token +[^- ]'; then echo "config: gateway.token set (never print it)"; else echo "config: gateway.token unset"; fi
else
    echo "kling: not installed"
    [ -x "$HOME/.local/bin/kling" ] && echo "kling: present at $HOME/.local/bin/kling but $HOME/.local/bin is not in PATH"
fi

sh_name="$(basename "${SHELL:-sh}")"
case "$sh_name" in
    bash) rc="$HOME/.bashrc" ;;
    zsh)  rc="$HOME/.zshrc" ;;
    fish) rc="$HOME/.config/fish/config.fish" ;;
    *)    rc="" ;;
esac
echo "shell: $sh_name"
if [ -n "$rc" ]; then
    target="$(readlink "$rc" 2>/dev/null)"
    if [ -e "$rc" ] && [ ! -w "$rc" ]; then
        echo "rc: $rc is READ-ONLY -> the installer will not edit it; it prints the lines to add by hand"
    elif [ -e "$rc" ] && [ "${target#/nix/store/}" != "$target" ]; then
        echo "rc: $rc is managed by home-manager (symlink to $target) -> the installer will not edit it; add the lines in your home-manager config (programs.zsh.initContent or your shell's equivalent)"
    elif [ -e "$rc" ]; then
        echo "rc: $rc writable"
    else
        echo "rc: $rc does not exist yet (the installer would create it)"
    fi
fi
case ":$PATH:" in *":$HOME/.local/bin:"*) echo "path: ~/.local/bin is in PATH" ;; *) echo "path: ~/.local/bin is NOT in PATH" ;; esac

if command -v chrono >/dev/null 2>&1; then echo "chrono: $(command -v chrono) $(chrono version 2>/dev/null | head -1)"; else echo "chrono: not installed"; fi
if command -v claude >/dev/null 2>&1; then echo "claude: $(command -v claude)"; else echo "claude: not in PATH (kling connect will patch ~/.claude.json directly)"; fi
command -v curl >/dev/null 2>&1 || echo "curl: not found (the installer needs curl or wget)"
exit 0
