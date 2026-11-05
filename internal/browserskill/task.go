package browserskill

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// TaskRunner executes browser commands on a connected device.
// It handles request-response correlation, timeouts, cancellation,
// and human-in-the-loop interruptions.
type TaskRunner struct {
	daemon *Daemon
	mu     sync.Mutex
	tasks  map[string]*pendingTask // keyed by task ID
}

type pendingTask struct {
	taskID   string
	conn     BrowserConnection
	resultCh chan *BrowserTaskResult
	cancel   context.CancelFunc
}

// NewTaskRunner creates a new task runner backed by the given daemon.
func NewTaskRunner(daemon *Daemon) *TaskRunner {
	return &TaskRunner{
		daemon: daemon,
		tasks:  make(map[string]*pendingTask),
	}
}

// RunCommand executes a single browser command and waits for the result.
// It finds an active browser for the user, sends the command, and blocks
// until the result is available, the context is cancelled, or the timeout
// is reached.
func (r *TaskRunner) RunCommand(ctx context.Context, tenantID uint64, userID string, command string, params interface{}) (*BrowserTaskResult, error) {
	// Find an active browser connection for this user
	conn, ok := r.daemon.FindUserConnection(tenantID, userID)
	if !ok {
		return nil, ErrBrowserNotConnected
	}

	taskID := generateTaskID()

	// Set up a timeout context
	taskCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	pending := &pendingTask{
		taskID:   taskID,
		conn:     conn,
		resultCh: make(chan *BrowserTaskResult, 1),
		cancel:   cancel,
	}

	r.mu.Lock()
	r.tasks[taskID] = pending
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.tasks, taskID)
		r.mu.Unlock()
	}()

	// Send the command
	msg := &BrowserMessage{
		Type:   "task_start",
		TaskID: taskID,
		Payload: map[string]interface{}{
			"command": command,
			"params":  params,
		},
	}
	if err := conn.Send(taskCtx, msg); err != nil {
		return nil, err
	}

	// Wait for result, cancellation, or connection close
	select {
	case result := <-pending.resultCh:
		return result, nil
	case <-taskCtx.Done():
		if taskCtx.Err() == context.DeadlineExceeded {
			return nil, ErrTaskTimeout
		}
		return nil, ErrTaskCancelled
	case <-conn.Done():
		return nil, ErrBrowserNotConnected
	}
}

// DeliverResult delivers a task result from the browser extension to the
// waiting caller. This is called by the WebSocket read loop when a
// task_result message arrives.
func (r *TaskRunner) DeliverResult(taskID string, result *BrowserTaskResult) bool {
	r.mu.Lock()
	pending, ok := r.tasks[taskID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case pending.resultCh <- result:
		return true
	default:
		return false
	}
}

// CancelTask cancels a pending task.
func (r *TaskRunner) CancelTask(taskID string) {
	r.mu.Lock()
	pending, ok := r.tasks[taskID]
	if ok {
		pending.cancel()
	}
	r.mu.Unlock()
}

// ActiveTaskCount returns the number of currently active tasks.
func (r *TaskRunner) ActiveTaskCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

// --- result payload parsing ---

// ParseTaskResultPayload extracts a BrowserTaskResult from a message payload.
// The payload may be either a raw JSON object or already deserialized as a map.
func ParseTaskResultPayload(payload interface{}) (*BrowserTaskResult, error) {
	// If it's already a map, re-marshal then unmarshal for type safety
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var result BrowserTaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- task ID generation ---

func generateTaskID() string {
	// Simple unique ID: timestamp + random suffix
	return "task_" + time.Now().Format("20060102150405") + "_" + randomHex(6)
}

func randomHex(n int) string {
	const chars = "abcdef0123456789"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = chars[int(now>>uint(i*4))&0xf]
	}
	return string(b)
}
