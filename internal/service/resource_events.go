package service

import "github.com/sabadia/dlm-server/internal/events"

// publishResource tells other clients that a resource changed (see
// events.TypeResourceChanged). It does nothing without a bus.
func publishResource(bus *events.Bus, kind, id, op string) {
	if bus == nil {
		return
	}
	bus.Publish(events.Event{Type: events.TypeResourceChanged, Data: events.ResourceChangedData{Kind: kind, ID: id, Op: op}})
}
