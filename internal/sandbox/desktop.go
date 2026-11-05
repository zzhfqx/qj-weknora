package sandbox

import (
	"context"
	"io"
	"net"
)

// DesktopConn is one bidirectional RFB stream between the server and a
// connected viewer. The handler copies frames in both directions until
// either side closes or the context is cancelled.
type DesktopConn interface {
	// Read reads one RFB frame (or partial frame) from the viewer.
	Read(p []byte) (int, error)
	// Write writes one RFB frame (or partial frame) to the viewer.
	Write(p []byte) (int, error)
	// Close terminates the connection.
	Close() error
}

// RemoteDesktopSession represents a running desktop session on the sandbox.
// The handler holds one of these per active WebSocket connection and uses it
// to relay RFB frames between the browser and the sandbox VNC server.
type RemoteDesktopSession interface {
	// Conn returns the raw byte stream for RFB frame relay.
	Conn() io.ReadWriteCloser
	// Close terminates the desktop session (or just the viewer connection,
	// depending on the backend).
	Close() error
}

// SessionDesktopManager opens VNC desktop sessions on the sandbox bound to
// a session. Like SessionTerminalManager it is provider-neutral: the
// WebSocket handler bridges noVNC frames to it without knowing whether
// Docker, Cube or E2B serves the session.
type SessionDesktopManager interface {
	// OpenSessionDesktop connects to the session's currently bound sandbox
	// and opens a VNC desktop session. It is lookup-only: when no live
	// sandbox is bound it returns ErrNoLiveSessionSandbox instead of
	// provisioning one. A backend that cannot serve desktop sessions
	// returns ErrDesktopUnsupported, not "no sandbox".
	OpenSessionDesktop(ctx context.Context, sessionID string) (RemoteDesktopSession, error)
}

// SessionDesktopProvider is implemented by managers that MAY offer
// graphical desktop sessions. The accessor returns nil when the current
// runtime cannot honour the capability.
type SessionDesktopProvider interface {
	SessionDesktopManager() SessionDesktopManager
}

// DesktopStartResult carries the outcome of a "start desktop" call. Some
// backends start the VNC server on demand (first viewer connect); others
// keep it running and just return an existing port.
type DesktopStartResult struct {
	// Port is the TCP port inside the sandbox where the VNC server is
	// listening. The relay dials localhost:Port inside the sandbox network.
	Port int
	// Started is true when this call caused the server to start, false
	// when it was already running.
	Started bool
}

// DesktopStatus reports whether a desktop server is running for a session
// and, if so, on what port.
type DesktopStatus struct {
	Running bool
	Port    int
}

// netConnDesktop wraps a net.Conn as a RemoteDesktopSession.
type netConnDesktop struct {
	conn net.Conn
}

// NewNetConnDesktop wraps a raw network connection so it satisfies the
// RemoteDesktopSession interface. Used by backends that dial the VNC
// server over TCP.
func NewNetConnDesktop(conn net.Conn) RemoteDesktopSession {
	return &netConnDesktop{conn: conn}
}

func (d *netConnDesktop) Conn() io.ReadWriteCloser { return d.conn }
func (d *netConnDesktop) Close() error             { return d.conn.Close() }
