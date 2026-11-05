package callctx

import (
	"context"
	"testing"
)

func TestWithKeepsWhatIsAlreadyKnown(t *testing.T) {
	ctx := With(t.Context(), Caller{Restricted: true})
	ctx = With(ctx, Caller{TaskID: "t1"})
	if got := From(ctx); got.TaskID != "t1" || !got.Restricted {
		t.Errorf("caller = %+v, want task t1 and restricted", got)
	}
	// Restriction is never lifted by a later caller.
	ctx = With(ctx, Caller{TaskID: "t2", Restricted: false})
	if got := From(ctx); got.TaskID != "t2" || !got.Restricted {
		t.Errorf("caller = %+v, want task t2 and still restricted", got)
	}
	if got := From(context.Background()); got != (Caller{}) {
		t.Errorf("caller of a bare context = %+v", got)
	}
}

func TestRouteSeesAProxyChosenLater(t *testing.T) {
	ctx, _ := WithRoute(t.Context())
	if Proxied(ctx) {
		t.Fatal("a new route is proxied")
	}
	MarkProxied(ctx)
	if !Proxied(ctx) {
		t.Error("the proxy was not noted")
	}
	MarkProxied(t.Context()) // no route: nothing to mark, no panic
}
