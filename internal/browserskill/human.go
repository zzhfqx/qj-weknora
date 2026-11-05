package browserskill

import (
	"context"
	"encoding/json"
)

// HumanHelpRequest represents a request for human assistance from the
// browser extension. When the agent encounters a situation it can't
// resolve (CAPTCHA, login wall, etc.), the extension can request help
// from the user.
type HumanHelpRequest struct {
	TaskID      string `json:"task_id"`
	SessionID   string `json:"session_id"`
	Reason      string `json:"reason"`       // why help is needed (e.g. "captcha", "login_required")
	Description string `json:"description"`  // human-readable description
	URL         string `json:"url"`          // current page URL
	Screenshot  string `json:"screenshot,omitempty"`
	State       string `json:"state,omitempty"` // opaque state blob for resuming
}

// HumanHelpResponse is sent back when the user resolves the help request.
type HumanHelpResponse struct {
	TaskID    string `json:"task_id"`
	Resolved  bool   `json:"resolved"`
	Feedback  string `json:"feedback,omitempty"`
	Continue  bool   `json:"continue"` // whether to continue the task
}

// HumanHelpManager coordinates human-in-the-loop interruptions.
type HumanHelpManager struct {
	store *DBStore
}

// NewHumanHelpManager creates a new human help manager.
func NewHumanHelpManager(store *DBStore) *HumanHelpManager {
	return &HumanHelpManager{store: store}
}

// SaveInterrupt persists a task interruption so it can be resumed later.
func (h *HumanHelpManager) SaveInterrupt(ctx context.Context, tenantID uint64, sessionID, taskID string, state interface{}) error {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return h.store.SaveTaskInterrupt(ctx, tenantID, sessionID, taskID, string(stateJSON))
}

// LoadInterrupt loads a previously saved interruption state.
func (h *HumanHelpManager) LoadInterrupt(ctx context.Context, tenantID uint64, sessionID, taskID string) (map[string]interface{}, error) {
	stateStr, err := h.store.GetTaskInterrupt(ctx, tenantID, sessionID, taskID)
	if err != nil {
		return nil, err
	}
	if stateStr == "" {
		return nil, ErrTaskNotFound
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
		return nil, err
	}
	return state, nil
}

// ResumeTask prepares a task result that tells the agent to resume from
// an interrupted state. This is returned when a task was interrupted
// and the user has resolved the issue.
func ResumeTask(interruptState map[string]interface{}) *BrowserTaskResult {
	return &BrowserTaskResult{
		Status:    TaskStatusPaused,
		HumanHelp: true,
	}
}

// IsHumanHelpError checks if an error is an ErrHumanHelpRequested sentinel.
func IsHumanHelpError(err error) bool {
	return err == ErrHumanHelpRequested
}
