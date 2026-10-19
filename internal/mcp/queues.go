package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

// QueueInfo is a queue as the tools report it.
type QueueInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state" jsonschema:"running or stopped"`
	IsDefault bool   `json:"isDefault,omitempty"`
}

func queueInfo(q *domain.Queue) QueueInfo {
	return QueueInfo{ID: q.ID, Name: q.Name, State: string(q.State), IsDefault: q.IsDefault}
}

type queueListOutput struct {
	Queues []QueueInfo `json:"queues"`
}

type createQueueInput struct {
	Name    string `json:"name" jsonschema:"queue name"`
	Stopped bool   `json:"stopped,omitempty" jsonschema:"create the queue stopped (it starts nothing until start_queue)"`
}

func (t *tools) registerQueues(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "list_queues", Description: "List the download queues.", Annotations: readOnly}, t.listQueues)
	sdk.AddTool(srv, &sdk.Tool{Name: "create_queue", Description: "Create a download queue.", Annotations: additive}, t.createQueue)
	sdk.AddTool(srv, &sdk.Tool{Name: "start_queue", Description: "Start a queue: its downloads run again.", Annotations: idempotent}, t.startQueue)
	sdk.AddTool(srv, &sdk.Tool{Name: "stop_queue", Description: "Stop a queue: its running downloads are paused and nothing new starts.", Annotations: idempotent}, t.stopQueue)
}

func (t *tools) listQueues(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, queueListOutput, error) {
	qs, err := t.d.Queues.List(ctx)
	if err != nil {
		return nil, queueListOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	out := queueListOutput{Queues: make([]QueueInfo, len(qs))}
	for i, q := range qs {
		out.Queues[i] = queueInfo(q)
	}
	return nil, out, nil
}

func (t *tools) createQueue(ctx context.Context, _ *sdk.CallToolRequest, in createQueueInput) (*sdk.CallToolResult, QueueInfo, error) {
	state := domain.QueueRunning
	if in.Stopped {
		state = domain.QueueStopped
	}
	q, err := t.d.Queues.Create(ctx, service.CreateQueueInput{Name: in.Name, State: state})
	if err != nil {
		return nil, QueueInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, queueInfo(q), nil
}

func (t *tools) startQueue(ctx context.Context, _ *sdk.CallToolRequest, in idInput) (*sdk.CallToolResult, QueueInfo, error) {
	q, err := t.d.Queues.Start(ctx, in.ID)
	if err != nil {
		return nil, QueueInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, queueInfo(q), nil
}

func (t *tools) stopQueue(ctx context.Context, _ *sdk.CallToolRequest, in idInput) (*sdk.CallToolResult, QueueInfo, error) {
	q, err := t.d.Queues.Stop(ctx, in.ID)
	if err != nil {
		return nil, QueueInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, queueInfo(q), nil
}
