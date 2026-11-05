package browserskill

import (
	"fmt"
	"strings"
)

// FormatTaskResult formats a BrowserTaskResult into a human-readable string
// for inclusion in the agent's context. This is the text the LLM sees when
// the browser tool returns.
func FormatTaskResult(result *BrowserTaskResult) string {
	if result == nil {
		return "[browser] no result"
	}

	var sb strings.Builder

	// Status line
	sb.WriteString(fmt.Sprintf("[browser] %s: %s\n", result.Status, result.URL))
	if result.Title != "" {
		sb.WriteString(fmt.Sprintf("Title: %s\n", result.Title))
	}
	if result.HumanHelp {
		sb.WriteString("Note: This task required human assistance to complete.\n")
	}
	if result.Error != "" {
		sb.WriteString(fmt.Sprintf("Error: %s\n", result.Error))
	}

	// Actions performed
	if len(result.Actions) > 0 {
		sb.WriteString("\nActions performed:\n")
		for _, a := range result.Actions {
			sb.WriteString(fmt.Sprintf("  - %s", a.Type))
			if a.Selector != "" {
				sb.WriteString(fmt.Sprintf(" (%s)", truncate(a.Selector, 80)))
			}
			if a.URL != "" {
				sb.WriteString(fmt.Sprintf(" → %s", truncate(a.URL, 80)))
			}
			sb.WriteString("\n")
		}
	}

	// Page content
	if result.Content != "" {
		sb.WriteString("\nPage content:\n")
		sb.WriteString("---\n")
		sb.WriteString(truncate(result.Content, 4000))
		if len(result.Content) > 4000 {
			sb.WriteString("\n... (content truncated)")
		}
		sb.WriteString("\n---\n")
	}

	// Screenshot info
	if result.Screenshot != "" {
		sb.WriteString("\n[Screenshot available]")
	}

	return sb.String()
}

// FormatErrorResult formats an error into a browser tool error response.
func FormatErrorResult(err error) string {
	return fmt.Sprintf("[browser] error: %s", err.Error())
}

// SummarizeResult creates a short one-line summary for display in the UI.
func SummarizeResult(result *BrowserTaskResult) string {
	if result == nil {
		return "No result"
	}
	switch result.Status {
	case TaskStatusCompleted:
		if result.Title != "" {
			return fmt.Sprintf("✓ %s", truncate(result.Title, 80))
		}
		return "✓ Task completed"
	case TaskStatusFailed:
		return fmt.Sprintf("✗ %s", truncate(result.Error, 80))
	case TaskStatusPaused:
		return "⏸ Waiting for input"
	case TaskStatusCancelled:
		return "⊘ Cancelled"
	case TaskStatusRunning:
		return "▶ Running..."
	case TaskStatusQueued:
		return "⏳ Queued..."
	default:
		return string(result.Status)
	}
}

// --- helpers ---

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
