# The README demo: records docs/demo.gif with charmbracelet VHS.
#
#   vhs docs/demo.tape            (from the repository root)
#
# Needs a Linux host with KVM where these already work, so the recording shows
# the steady state and not the one-time downloads and builds:
#   - kindling with the sandbox extension and the daemon running
#     (install.sh --with sandbox, sudo kling up -install), and `kling try` run
#     once so the kernel and `min` are in place;
#   - the `toolchain` image and the `python` template built
#     (kling image toolchain; kling sbx template apply -f examples/sandbox/templates/python.json);
#   - no machine called `demo` (the tape creates it and removes it at the end).
# The tape starts its own gateway with a throwaway token and stops it at the end.

Output docs/demo.gif

Require kling
Require openssl

Set Shell "bash"
Set FontSize 18
Set Width 1200
Set Height 640
Set Padding 24
Set TypingSpeed 35ms
Set Theme "Builtin Dark"

# Off camera: a clean prompt, a token and the sandbox gateway.
Hide
Type `export PS1='$ ' KLING_SANDBOX_TOKEN=$(openssl rand -hex 24)` Enter
Type `kling sbx gateway >/tmp/kling-demo-gateway.log 2>&1 & sleep 2; clear` Enter
Show

# 1. A throwaway microVM: create, run, print, delete.
Type "# a throwaway microVM: its own kernel, gone when the command ends" Enter
Type "kling try -- uname -a" Enter
Sleep 4s

# 2. A machine, frozen to disk and thawed: kling prints how long each took.
Type "# boot one, freeze it to disk (0 CPU, 0 RAM), thaw it" Enter
Type "kling run -name demo" Enter
Sleep 3s
Type "kling freeze demo" Enter
Sleep 2s
Type "kling thaw demo" Enter
Sleep 3s

# 3. A sandbox through the gateway, from the template's warm pool, and code in it.
Type "# a sandbox for an agent: from the template's warm pool" Enter
Type "kling sbx new -template python -ttl 10m" Enter
Sleep 2s
Hide
Type `ID=$(kling sbx ls -json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')` Enter
Show
Type `kling sbx exec $ID -- python3 -c 'print(6*7)'` Enter
Sleep 3s

# Off camera: clean up.
Hide
Type "kling sbx rm $ID; kling rm demo; kill %1" Enter
Sleep 1s
Show
Sleep 1s
