#!/usr/bin/env bash
# Start a lightweight XFCE desktop inside the sandbox container.
#
# Stack:
#   Xvfb          → virtual X11 display (no physical monitor)
#   xfce4-session → desktop environment
#   x11vnc        → VNC server that shares the Xvfb display
#   websockify    → WebSocket bridge (noVNC protocol) on port 6080
#
# The frontend noVNC client connects to /sessions/:id/sandbox/desktop,
# which the backend relays to localhost:6080 inside the sandbox.

set -euo pipefail

DISPLAY_NUM="${DISPLAY_NUM:-99}"
VNC_PORT="${VNC_PORT:-5900}"
WEBSOCK_PORT="${WEBSOCK_PORT:-6080}"
SCREEN_RESOLUTION="${SCREEN_RESOLUTION:-1280x800x24}"
DESKTOP_USER="${DESKTOP_USER:-root}"

export DISPLAY=":${DISPLAY_NUM}"

# ---- Already running? -----------------------------------------------------

if pgrep -f "Xvfb.*:${DISPLAY_NUM}" > /dev/null 2>&1; then
    echo "[desktop] Xvfb already running on :${DISPLAY_NUM}"
    exit 0
fi

# ---- Xvfb -----------------------------------------------------------------

echo "[desktop] Starting Xvfb on ${DISPLAY} (${SCREEN_RESOLUTION})"
Xvfb "${DISPLAY}" -screen 0 "${SCREEN_RESOLUTION}" -ac +extension GLX +render -noreset &
XVFB_PID=$!

# Wait for the display to come up
for i in $(seq 1 30); do
    if xdpyinfo -display "${DISPLAY}" > /dev/null 2>&1; then
        break
    fi
    sleep 0.2
done

if ! xdpyinfo -display "${DISPLAY}" > /dev/null 2>&1; then
    echo "[desktop] ERROR: Xvfb failed to start" >&2
    exit 1
fi

# ---- Desktop environment --------------------------------------------------

echo "[desktop] Starting XFCE session"
# XFCE config: if a custom config was uploaded, use it
if [ -f /tmp/desktop-setup/xfce4-desktop.xml ]; then
    mkdir -p "/${DESKTOP_USER}/.config/xfce4/xfconf/xfce-perchannel-xml"
    cp /tmp/desktop-setup/xfce4-desktop.xml \
       "/${DESKTOP_USER}/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml"
fi

sudo -u "${DESKTOP_USER}" xfce4-session &
XFCE_PID=$!

# ---- VNC server (x11vnc) -------------------------------------------------

echo "[desktop] Starting x11vnc on port ${VNC_PORT}"
x11vnc -display "${DISPLAY}" \
       -rfbport "${VNC_PORT}" \
       -nopw \
       -shared \
       -forever \
       -bg \
       -o /tmp/x11vnc.log > /dev/null 2>&1

# Wait for VNC server to be ready
for i in $(seq 1 30); do
    if echo "" | nc -w 1 localhost "${VNC_PORT}" > /dev/null 2>&1; then
        break
    fi
    sleep 0.2
done

# ---- WebSocket bridge (websockify) ---------------------------------------

echo "[desktop] Starting websockify on port ${WEBSOCK_PORT}"
websockify --web /usr/share/novnc "${WEBSOCK_PORT}" "localhost:${VNC_PORT}" &
WEBSOCK_PID=$!

# Wait for websockify to be ready
for i in $(seq 1 30); do
    if echo "" | nc -w 1 localhost "${WEBSOCK_PORT}" > /dev/null 2>&1; then
        break
    fi
    sleep 0.2
done

echo "[desktop] Desktop ready: VNC=${VNC_PORT} WebSocket=${WEBSOCK_PORT}"
echo "[desktop]   Xvfb PID:  ${XVFB_PID}"
echo "[desktop]   XFCE PID:  ${XFCE_PID}"
echo "[desktop]   websockify PID: ${WEBSOCK_PID}"
