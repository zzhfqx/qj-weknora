package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
)

// DesktopWebSocketDialer opens a WebSocket connection to the noVNC
// websockify endpoint inside a sandbox. Different backends reach their
// sandbox via different transport (direct TCP, envd exec proxy, etc.),
// so each backend provides its own dialer.
type DesktopWebSocketDialer interface {
	// DialDesktopWS opens a WebSocket connection to the VNC websockify
	// endpoint inside the session's sandbox. The returned connection is
	// ready to relay RFB frames.
	DialDesktopWS(ctx context.Context, sessionID string) (*websocket.Conn, error)
}

// DirectTCPDesktopDialer dials a VNC websockify server directly over TCP.
// Used by backends that have direct network access to the sandbox
// (e.g. Docker on the same host).
type DirectTCPDesktopDialer struct {
	// Host is the sandbox hostname or IP address.
	Host string
	// Port is the websockify port inside the sandbox.
	Port int
}

// NewDirectTCPDesktopDialer creates a dialer that connects to a
// websockify server at host:port directly over TCP.
func NewDirectTCPDesktopDialer(host string, port int) *DirectTCPDesktopDialer {
	return &DirectTCPDesktopDialer{Host: host, Port: port}
}

func (d *DirectTCPDesktopDialer) DialDesktopWS(ctx context.Context, _ string) (*websocket.Conn, error) {
	url := fmt.Sprintf("ws://%s:%d/websockify", d.Host, d.Port)
	dialer := websocket.Dialer{
		Subprotocols: []string{"binary"},
	}
	conn, _, err := dialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("dial desktop websocket: %w", err)
	}
	return conn, nil
}

// wsDesktopConn wraps a gorilla/websocket.Conn as a RemoteDesktopSession.
// RFB frames are sent as binary WebSocket messages, matching what
// noVNC / websockify expect.
type wsDesktopConn struct {
	conn *websocket.Conn
}

// NewWebSocketDesktop wraps a gorilla WebSocket connection as a
// RemoteDesktopSession. Binary message type is used for RFB frame relay.
func NewWebSocketDesktop(conn *websocket.Conn) RemoteDesktopSession {
	return &wsDesktopConn{conn: conn}
}

func (d *wsDesktopConn) Close() error {
	return d.conn.Close()
}

// wsReadWriter adapts a websocket.Conn into io.ReadWriter by reading
// full messages into a buffer and draining them on Read calls.
type wsReadWriter struct {
	conn  *websocket.Conn
	frame []byte
	pos   int
}

func (rw *wsReadWriter) Read(p []byte) (int, error) {
	if rw.pos >= len(rw.frame) {
		// Read next binary message
		msgType, data, err := rw.conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		if msgType != websocket.BinaryMessage {
			// Skip text messages (noVNC heartbeats etc.)
			return rw.Read(p)
		}
		rw.frame = data
		rw.pos = 0
	}
	n := copy(p, rw.frame[rw.pos:])
	rw.pos += n
	return n, nil
}

func (rw *wsReadWriter) Write(p []byte) (int, error) {
	err := rw.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (rw *wsReadWriter) Close() error {
	return rw.conn.Close()
}

func (d *wsDesktopConn) Conn() io.ReadWriteCloser {
	return &wsReadWriter{conn: d.conn}
}
