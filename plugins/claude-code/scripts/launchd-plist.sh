#!/bin/sh
# launchd-plist.sh — prints the launchd agent that starts `kling daemon` at
# login on macOS (docs/mac.md), with absolute paths: launchd expands neither
# ~ nor $HOME. Usage:
#   sh launchd-plist.sh [/path/to/kling] > ~/Library/LaunchAgents/dev.kindling.daemon.plist
#   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.kindling.daemon.plist
KLING="${1:-$(command -v kling 2>/dev/null || echo "$HOME/.local/bin/kling")}"
LOG="$HOME/Library/Logs/kindling.log"
cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.kindling.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>$KLING</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>$LOG</string>
  <key>StandardErrorPath</key>
  <string>$LOG</string>
</dict>
</plist>
EOF
