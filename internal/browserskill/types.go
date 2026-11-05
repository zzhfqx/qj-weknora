package browserskill

// BrowserTaskStatus represents the lifecycle state of a browser task.
type BrowserTaskStatus string

const (
	TaskStatusQueued    BrowserTaskStatus = "queued"
	TaskStatusRunning   BrowserTaskStatus = "running"
	TaskStatusPaused    BrowserTaskStatus = "paused"    // waiting for human input
	TaskStatusCompleted BrowserTaskStatus = "completed"
	TaskStatusFailed    BrowserTaskStatus = "failed"
	TaskStatusCancelled BrowserTaskStatus = "cancelled"
)

// BrowserTaskResult holds the output of a completed browser task.
type BrowserTaskResult struct {
	TaskID      string             `json:"task_id"`
	Status      BrowserTaskStatus  `json:"status"`
	URL         string             `json:"url"`
	Title       string             `json:"title"`
	Screenshot  string             `json:"screenshot,omitempty"`  // base64-encoded PNG
	Content     string             `json:"content,omitempty"`     // page text / DOM snapshot
	Actions     []TaskAction       `json:"actions,omitempty"`     // actions performed
	Error       string             `json:"error,omitempty"`
	HumanHelp   bool               `json:"human_help"`            // required human assistance
	Interactive bool               `json:"interactive"`           // required user interaction
}

// TaskAction describes one atomic action performed during a task.
type TaskAction struct {
	Type      string `json:"type"`      // navigate, click, type, scroll, etc.
	Selector  string `json:"selector,omitempty"`
	Text      string `json:"text,omitempty"`
	URL       string `json:"url,omitempty"`
	Timestamp int64  `json:"ts"`
}

// ToolCall represents an agent's request to invoke a browser sub-command.
type ToolCall struct {
	Command string      `json:"command"`  // e.g. "navigate", "click", "type", "screenshot"
	Params  interface{} `json:"params"`
}

// BrowserMessage is the wire format for browser extension ↔ server messages.
type BrowserMessage struct {
	Type    string      `json:"type"`    // "task_start", "task_result", "ping", "pong", "pair", etc.
	TaskID  string      `json:"task_id,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
	Error   string      `json:"error,omitempty"`
}
