package api

import (
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/events"
)

// eventSchemaName turns an event type into the name of its schema:
// "task.status" is "TaskStatusEvent", "queue.action_cancelled" is
// "QueueActionCancelledEvent".
func eventSchemaName(t events.Type) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(string(t), func(r rune) bool { return r == '.' || r == '_' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String() + "Event"
}

// registerEventSchemas adds one schema per event type, whose data property has
// the type the event really carries, and the StreamEvent schema that is their
// oneOf discriminated by type. It returns a reference to StreamEvent. A client
// generator therefore gets a sealed union with typed data instead of "any".
func registerEventSchemas(reg huma.Registry) *huma.Schema {
	const prefix = "#/components/schemas/"
	var variants []*huma.Schema
	mapping := map[string]string{}
	for _, k := range events.Catalogue() {
		name := eventSchemaName(k.Type)
		reg.Map()[name] = &huma.Schema{
			Type:        huma.TypeObject,
			Description: k.Doc,
			Properties: map[string]*huma.Schema{
				"id":      {Type: huma.TypeInteger, Format: "int64", Description: "Bus sequence number; the SSE id is <bootID>-<id>."},
				"type":    {Type: huma.TypeString, Enum: []any{string(k.Type)}, Description: "The event type; also the SSE event name."},
				"time":    {Type: huma.TypeString, Format: "date-time", Description: "When the event was emitted (UTC)."},
				"taskId":  {Type: huma.TypeString, Description: "The task the event is about, if any."},
				"queueId": {Type: huma.TypeString, Description: "The queue the event is about, if any."},
				"data":    reg.Schema(reflect.TypeOf(k.Payload), true, ""),
			},
			Required:             []string{"id", "type", "time", "data"},
			AdditionalProperties: false,
		}
		variants = append(variants, &huma.Schema{Ref: prefix + name})
		mapping[string(k.Type)] = prefix + name
	}
	reg.Map()["StreamEvent"] = &huma.Schema{
		Description:   "An event of the stream: one of the typed events, told apart by type.",
		OneOf:         variants,
		Discriminator: &huma.Discriminator{PropertyName: "type", Mapping: mapping},
	}
	return &huma.Schema{Ref: prefix + "StreamEvent"}
}
