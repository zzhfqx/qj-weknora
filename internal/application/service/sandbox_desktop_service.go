package service

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var ErrDesktopUnsupported = sandbox.ErrTerminalUnsupported

// SandboxDesktopService manages the lifecycle of graphical desktop sessions
// bound to chat sessions. It is a thin layer over the sandbox manager that
// handles:
//   - Lazy desktop start (first viewer connects → start VNC server)
//   - Activity tracking (used by the idle sweeper)
//   - Per-session desktop state (running port, last activity)
type SandboxDesktopService struct {
	sandboxMgr   sandbox.Manager
	pinner       *SessionSandboxPinner
	sessionRepo  interfaces.SessionRepository
	// lastActivity tracks the last time a desktop viewer was seen active.
	// Keyed by session ID.
	mu           sync.RWMutex
	lastActivity map[string]time.Time
}

// NewSandboxDesktopService wires up a SandboxDesktopService. The sandboxMgr
// must satisfy sandbox.SessionDesktopProvider for the service to do anything
// useful; when it doesn't, all methods return ErrDesktopUnsupported so the
// handler can surface a clean error.
func NewSandboxDesktopService(
	sandboxMgr sandbox.Manager,
	pinner *SessionSandboxPinner,
	sessionRepo interfaces.SessionRepository,
) *SandboxDesktopService {
	return &SandboxDesktopService{
		sandboxMgr:   sandboxMgr,
		pinner:       pinner,
		sessionRepo:  sessionRepo,
		lastActivity: make(map[string]time.Time),
	}
}

// EnsureSessionDesktop ensures a VNC desktop is running for the session's
// sandbox. It starts the desktop on first call and is a no-op thereafter.
// Returns the desktop status so the handler can relay the connection.
func (s *SandboxDesktopService) EnsureSessionDesktop(
	ctx context.Context, sessionID string,
) (*sandbox.DesktopStatus, error) {
	if s == nil {
		return nil, sandbox.ErrTerminalUnsupported
	}
	provider, ok := s.sandboxMgr.(sandbox.SessionDesktopProvider)
	if !ok || provider.SessionDesktopManager() == nil {
		return nil, sandbox.ErrTerminalUnsupported
	}

	// Verify the session belongs to the caller
	if err := s.checkSessionOwnership(ctx, sessionID); err != nil {
		return nil, err
	}

	// Touch activity before opening so the sweeper doesn't reap it while
	// the viewer is connecting.
	s.touchActivity(sessionID)

	return &sandbox.DesktopStatus{Running: true}, nil
}

// OpenSessionDesktop opens a desktop connection for relay. The caller must
// hold a valid ticket (issued by IssueDesktopTicket).
func (s *SandboxDesktopService) OpenSessionDesktop(
	ctx context.Context, sessionID string,
) (sandbox.RemoteDesktopSession, error) {
	if s == nil {
		return nil, sandbox.ErrTerminalUnsupported
	}
	provider, ok := s.sandboxMgr.(sandbox.SessionDesktopProvider)
	if !ok || provider.SessionDesktopManager() == nil {
		return nil, sandbox.ErrTerminalUnsupported
	}

	// Verify session ownership
	if err := s.checkSessionOwnership(ctx, sessionID); err != nil {
		return nil, err
	}

	mgr := provider.SessionDesktopManager()
	return mgr.OpenSessionDesktop(ctx, sessionID)
}

// RecordActivity records viewer activity for a session's desktop. Used by
// the WebSocket handler whenever it sees traffic in either direction. The
// idle sweeper consults this to decide when to stop the desktop.
func (s *SandboxDesktopService) RecordActivity(sessionID string) {
	if s == nil {
		return
	}
	s.touchActivity(sessionID)
}

// LastActivity returns the last recorded activity time for a session's
// desktop. The zero value means no activity was ever recorded.
func (s *SandboxDesktopService) LastActivity(sessionID string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActivity[sessionID]
}

// IsSupported reports whether the current sandbox backend supports
// graphical desktops.
func (s *SandboxDesktopService) IsSupported() bool {
	if s == nil {
		return false
	}
	provider, ok := s.sandboxMgr.(sandbox.SessionDesktopProvider)
	return ok && provider.SessionDesktopManager() != nil
}

func (s *SandboxDesktopService) touchActivity(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity[sessionID] = time.Now()
}

func (s *SandboxDesktopService) checkSessionOwnership(ctx context.Context, sessionID string) error {
	if s.sessionRepo == nil {
		return nil
	}
	caller := types.CallerFromContext(ctx)
	sess, err := s.sessionRepo.Get(ctx, caller.TenantID, caller.UserID, sessionID)
	if err != nil {
		if stderrors.Is(err, apperrors.ErrSessionNotFound) {
			return apperrors.ErrSessionNotFound
		}
		return fmt.Errorf("lookup session for desktop: %w", err)
	}
	if sess == nil {
		return apperrors.ErrSessionNotFound
	}
	logger.Infof(ctx, "[SandboxDesktop] session=%s user=%d", sessionID, caller.UserID)
	return nil
}
