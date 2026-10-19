package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// writableSettings are the top-level settings an assistant may change. The
// others (post actions run programs, proxies and headers redirect or label
// traffic, directories choose where files are written) stay with the user.
var writableSettings = []string{"speedLimit", "maxConnections", "retry", "fileExistsPolicy", "autoCategorize"}

type getSettingsInput struct {
	TaskID  string `json:"taskId,omitempty" jsonschema:"show the settings in effect for this download"`
	QueueID string `json:"queueId,omitempty" jsonschema:"show the settings in effect for this queue"`
}

type settingsOutput struct {
	Scope    string         `json:"scope" jsonschema:"global, queue or task"`
	Settings map[string]any `json:"settings" jsonschema:"the settings; for a queue or task each value is paired with the level it comes from"`
}

type updateSettingsInput struct {
	Patch map[string]any `json:"patch" jsonschema:"JSON merge patch of the global settings; null removes a setting. Allowed keys: speedLimit (bytes/s, 0 = unlimited), maxConnections, retry, fileExistsPolicy, autoCategorize"`
}

func (t *tools) registerSettings(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "get_settings", Annotations: readOnly,
		Description: "Show the global settings, or the settings in effect for a queue or a download (with where each value comes from).",
	}, t.getSettings)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "update_settings", Annotations: idempotent,
		Description: "Change global settings (speed limit, connections per download, retries, file-exists policy, auto-categorizing).",
	}, t.updateSettings)
}

// asObject turns v into the generic JSON object the tool output carries.
func asObject(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	return m, nil
}

func (t *tools) getSettings(ctx context.Context, _ *sdk.CallToolRequest, in getSettingsInput) (*sdk.CallToolResult, settingsOutput, error) {
	var (
		scope string
		v     any
		err   error
	)
	switch {
	case in.TaskID != "" && in.QueueID != "":
		return nil, settingsOutput{}, fmt.Errorf("give taskId or queueId, not both")
	case in.TaskID != "":
		scope = "task"
		v, err = t.d.Settings.EffectiveForTask(ctx, in.TaskID)
	case in.QueueID != "":
		scope = "queue"
		v, err = t.d.Settings.EffectiveForQueue(ctx, in.QueueID)
	default:
		scope = "global"
		v, err = t.d.Settings.Global(ctx)
	}
	if err != nil {
		return nil, settingsOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	obj, err := asObject(v)
	if err != nil {
		return nil, settingsOutput{}, err
	}
	return nil, settingsOutput{Scope: scope, Settings: obj}, nil
}

func (t *tools) updateSettings(ctx context.Context, _ *sdk.CallToolRequest, in updateSettingsInput) (*sdk.CallToolResult, settingsOutput, error) {
	if len(in.Patch) == 0 {
		return nil, settingsOutput{}, fmt.Errorf("patch is empty")
	}
	var denied []string
	for k := range in.Patch {
		if !slices.Contains(writableSettings, k) {
			denied = append(denied, k)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return nil, settingsOutput{}, fmt.Errorf("these settings cannot be changed through MCP: %s (allowed: %s)",
			strings.Join(denied, ", "), strings.Join(writableSettings, ", "))
	}
	patch, err := json.Marshal(in.Patch)
	if err != nil {
		return nil, settingsOutput{}, fmt.Errorf("encode patch: %w", err)
	}
	g, err := t.d.Settings.PatchGlobal(ctx, patch)
	if err != nil {
		return nil, settingsOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	obj, err := asObject(g)
	if err != nil {
		return nil, settingsOutput{}, err
	}
	return nil, settingsOutput{Scope: "global", Settings: obj}, nil
}
