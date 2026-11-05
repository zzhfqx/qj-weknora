package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/browserskill"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

const ToolBrowser = "browser"

// browserTool is the base definition. The description is intentionally
// thorough — the LLM uses it to decide when to invoke the tool.
var browserTool = BaseTool{
	name:        ToolBrowser,
	description: browserToolDescription,
	schema:      utils.GenerateSchema[BrowserToolInput](),
}

const browserToolDescription = `Drive a real local browser (Chrome/Edge/etc.) on the user's machine via a paired browser extension.
Use this for:
- Navigating to specific URLs and extracting page content
- Filling out forms, clicking buttons, and interacting with web UIs
- Taking screenshots of pages or specific elements
- Browsing sites that require authentication (the user's browser session is used)
- Testing web applications interactively
- Accessing content that is only available in a real browser (JS-rendered pages, SPAs)

The browser is the user's own browser — it uses their cookies, logins, and bookmarks.
Never use this tool for:
- Accessing the user's private data without explicit permission
- Making purchases or financial transactions
- Modifying the user's browser settings or extensions
- Uploading or downloading files without explicit consent

Available commands:
- navigate: Go to a URL
- click: Click an element by selector or text
- type: Type text into an input field
- screenshot: Take a screenshot of the current page
- get_content: Get the text/DOM content of the current page
- scroll: Scroll the page
- back: Navigate back
- forward: Navigate forward
- refresh: Refresh the page

Each command runs in the user's active browser tab. Results include the page URL,
title, and relevant content. Screenshots are returned as base64 PNG data.`

// BrowserToolInput is the JSON-schema shape for browser tool parameters.
type BrowserToolInput struct {
	// Command is the browser action to perform.
	Command string `json:"command" jsonschema:"Browser command to execute: navigate, click, type, screenshot, get_content, scroll, back, forward, refresh"`
	// URL is the target URL (for "navigate").
	URL string `json:"url,omitempty" jsonschema:"URL to navigate to (required for 'navigate' command)"`
	// Selector is a CSS selector for the target element (for "click", "type", etc.).
	Selector string `json:"selector,omitempty" jsonschema:"CSS selector for the target element (used by click, type commands)"`
	// Text is the text to type or look for (for "type" or text-based click).
	Text string `json:"text,omitempty" jsonschema:"Text content to type or match (used by type, click commands)"`
	// Direction is the scroll direction (for "scroll"): "up" or "down".
	Direction string `json:"direction,omitempty" jsonschema:"Scroll direction: 'up' or 'down' (for 'scroll' command)"`
	// Amount is how much to scroll in pixels (for "scroll").
	Amount int `json:"amount,omitempty" jsonschema:"Pixels to scroll (for 'scroll' command, default 500)"`
	// FullPage controls whether to capture the full page (for "screenshot").
	FullPage bool `json:"full_page,omitempty" jsonschema:"Capture full page screenshot (for 'screenshot' command, default false)"`
}

// BrowserTool executes browser automation commands on a paired local browser.
type BrowserTool struct {
	BaseTool
	taskRunner *browserskill.TaskRunner
}

// Compile-time check that BrowserTool implements types.Tool.
var _ types.Tool = (*BrowserTool)(nil)

// NewBrowserTool creates a new browser automation tool.
func NewBrowserTool(taskRunner *browserskill.TaskRunner) *BrowserTool {
	return &BrowserTool{
		BaseTool:   browserTool,
		taskRunner: taskRunner,
	}
}

// Execute runs the browser tool.
func (t *BrowserTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input BrowserToolInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("invalid arguments: %v", err),
			Output:  "Error: invalid browser tool arguments",
		}, nil
	}

	if input.Command == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "command is required",
			Output:  "Error: 'command' parameter is required",
		}, nil
	}

	if t.taskRunner == nil {
		return &types.ToolResult{
			Success: false,
			Error:   browserskill.ErrBrowserUnavailable.Error(),
			Output:  "Browser skill is not available in this deployment.",
		}, nil
	}

	// Get caller info from context
	caller := types.CallerFromContext(ctx)
	if caller.TenantID == 0 || caller.UserID == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "no caller context",
			Output:  "Error: missing caller context",
		}, nil
	}

	logger.Infof(ctx, "[browser tool] command=%s url=%s selector=%s",
		input.Command, input.URL, input.Selector)

	// Build params map for the runner
	params := map[string]interface{}{
		"command": input.Command,
	}
	if input.URL != "" {
		params["url"] = input.URL
	}
	if input.Selector != "" {
		params["selector"] = input.Selector
	}
	if input.Text != "" {
		params["text"] = input.Text
	}
	if input.Direction != "" {
		params["direction"] = input.Direction
	}
	if input.Amount > 0 {
		params["amount"] = input.Amount
	}
	if input.FullPage {
		params["full_page"] = input.FullPage
	}

	// Execute the command on the paired browser
	result, err := t.taskRunner.RunCommand(ctx, caller.TenantID, caller.UserID, input.Command, params)
	if err != nil {
		logger.Warnf(ctx, "[browser tool] command failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   err.Error(),
			Output:  fmt.Sprintf("Browser error: %s", err.Error()),
		}, nil
	}

	// Format output
	output := browserskill.FormatTaskResult(result)

	toolResult := &types.ToolResult{
		Success: result.Status == browserskill.TaskStatusCompleted,
		Output:  output,
		Data: map[string]interface{}{
			"display_type": "browser",
			"url":          result.URL,
			"title":        result.Title,
			"status":       string(result.Status),
			"human_help":   result.HumanHelp,
			"interactive":  result.Interactive,
			"content":      result.Content,
			"screenshot":   result.Screenshot,
		},
	}

	if result.Error != "" {
		toolResult.Error = result.Error
		toolResult.Data["error"] = result.Error
	}

	if result.Screenshot != "" {
		toolResult.Images = []string{result.Screenshot}
	}

	return toolResult, nil
}
