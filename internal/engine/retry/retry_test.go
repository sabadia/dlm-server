package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

func fixed(r float64) func() float64 { return func() float64 { return r } }

func TestFromSettings(t *testing.T) {
	got := FromSettings(settings.RetrySettings{})
	want := Policy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: time.Minute, RespectRetryAfter: true, MaxRetryAfter: 10 * time.Minute}
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b func() float64) bool { return a == nil && b == nil })); diff != "" {
		t.Errorf("defaults (-want +got):\n%s", diff)
	}
	got = FromSettings(settings.RetrySettings{
		MaxAttempts:       settings.Some(0),
		InitialBackoff:    settings.Some(settings.Duration(2 * time.Second)),
		MaxBackoff:        settings.Some(settings.Duration(30 * time.Second)),
		RespectRetryAfter: settings.Some(false),
	})
	if got.MaxAttempts != 0 || got.InitialBackoff != 2*time.Second || got.MaxBackoff != 30*time.Second || got.RespectRetryAfter {
		t.Errorf("explicit settings not applied: %+v", got)
	}
}

func TestBackoff(t *testing.T) {
	p := Policy{InitialBackoff: time.Second, MaxBackoff: time.Minute, Rand: fixed(1)}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{6, 32 * time.Second},
		{7, time.Minute},
		{40, time.Minute},
		{63, time.Minute},
		{64, time.Minute},
		{1 << 30, time.Minute},
	}
	for _, tt := range tests {
		if got := p.Backoff(tt.attempt); got != tt.want {
			t.Errorf("Backoff(%d) with rand=1 = %v, want %v", tt.attempt, got, tt.want)
		}
	}

	huge := Policy{InitialBackoff: time.Duration(1 << 61), MaxBackoff: time.Duration(1<<63 - 1), Rand: fixed(1)}
	if got := huge.Backoff(5); got <= 0 {
		t.Errorf("overflowed: Backoff = %v", got)
	}

	for _, r := range []float64{0, 0.25, 0.5, 0.999} {
		p.Rand = fixed(r)
		if got, want := p.Backoff(3), time.Duration(float64(4*time.Second)*r); got != want {
			t.Errorf("jitter r=%v: got %v, want %v", r, got, want)
		}
	}
	for _, r := range []float64{-1, 2} {
		p.Rand = fixed(r)
		if got := p.Backoff(3); got < 0 || got > 4*time.Second {
			t.Errorf("out-of-range rand %v gave %v", r, got)
		}
	}

	// Default random source stays within bounds.
	p.Rand = nil
	for i := range 200 {
		if got := p.Backoff(4); got < 0 || got > 8*time.Second {
			t.Fatalf("iteration %d: Backoff(4) = %v out of [0, 8s]", i, got)
		}
	}
}

func TestDelay(t *testing.T) {
	ra := func(d time.Duration) error {
		e := engine.Transient(domain.ErrorCodeHTTPStatus, errors.New("503"))
		e.RetryAfter = d
		return e
	}
	base := Policy{InitialBackoff: time.Second, MaxBackoff: time.Minute, RespectRetryAfter: true, MaxRetryAfter: 5 * time.Minute, Rand: fixed(1)}
	disabled := base
	disabled.RespectRetryAfter = false
	tests := []struct {
		name    string
		p       Policy
		attempt int
		err     error
		want    time.Duration
	}{
		{"retry-after longer than backoff", base, 1, ra(30 * time.Second), 30 * time.Second},
		{"backoff longer than retry-after", base, 4, ra(time.Second), 8 * time.Second},
		{"capped", base, 1, ra(time.Hour), 5 * time.Minute},
		{"disabled", disabled, 1, ra(30 * time.Second), time.Second},
		{"no retry-after", base, 2, ra(0), 2 * time.Second},
		{"plain error", base, 2, errors.New("x"), 2 * time.Second},
		{"default cap", Policy{RespectRetryAfter: true, Rand: fixed(0)}, 1, ra(time.Hour), DefaultMaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Delay(tt.attempt, tt.err); got != tt.want {
				t.Errorf("Delay = %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeSleeper struct {
	delays []time.Duration
	// cancel, when set, is called on the given wait (1-based) and the wait
	// then reports the context error.
	cancelOn int
	cancel   context.CancelFunc
}

func (f *fakeSleeper) sleep(ctx context.Context, d time.Duration) error {
	f.delays = append(f.delays, d)
	if f.cancel != nil && len(f.delays) == f.cancelOn {
		f.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

var (
	errTransient = engine.Transient(domain.ErrorCodeNetwork, errors.New("reset"))
	errFatal     = engine.Fatal(domain.ErrorCodeHTTPStatus, errors.New("400"))
	errExpired   = engine.LinkExpired(errors.New("403"))
)

func failN(n int, err error) (func(context.Context, int) error, *[]int) {
	var calls []int
	return func(_ context.Context, attempt int) error {
		calls = append(calls, attempt)
		if len(calls) <= n {
			return err
		}
		return nil
	}, &calls
}

func TestDo(t *testing.T) {
	policy := Policy{MaxAttempts: 3, InitialBackoff: time.Second, MaxBackoff: 10 * time.Second, Rand: fixed(1)}
	tests := []struct {
		name      string
		failures  int
		err       error
		wantCalls int
		wantErr   error
		wantKind  engine.Kind
		wantWaits []time.Duration
	}{
		{"success first try", 0, errTransient, 1, nil, 0, nil},
		{"success after transient failures", 3, errTransient, 4, nil, 0, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}},
		{"gives up at MaxAttempts", 10, errTransient, 4, errTransient, engine.KindTransient, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}},
		{"fatal not retried", 10, errFatal, 1, errFatal, engine.KindFatal, nil},
		{"link expired not retried", 10, errExpired, 1, errExpired, engine.KindLinkExpired, nil},
		{"canceled not retried", 10, context.Canceled, 1, context.Canceled, engine.KindCanceled, nil},
		{"unclassified not retried", 10, errors.New("raw"), 1, nil, engine.KindFatal, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, calls := failN(tt.failures, tt.err)
			fs := &fakeSleeper{}
			err := Do(context.Background(), policy, op, WithSleeper(fs.sleep))
			if len(*calls) != tt.wantCalls {
				t.Errorf("calls = %v, want %d", *calls, tt.wantCalls)
			}
			for i, a := range *calls {
				if a != i+1 {
					t.Errorf("attempt numbers = %v, want 1..n", *calls)
					break
				}
			}
			if diff := cmp.Diff(tt.wantWaits, fs.delays); diff != "" {
				t.Errorf("waits (-want +got):\n%s", diff)
			}
			if tt.failures == 0 || (tt.wantCalls > tt.failures) {
				if err != nil {
					t.Fatalf("Do = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Do = nil, want error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Do = %v, want wrapping %v", err, tt.wantErr)
			}
			if k := engine.Classify(err); k != tt.wantKind {
				t.Errorf("Classify(Do) = %v, want %v", k, tt.wantKind)
			}
		})
	}
}

func TestDoZeroAttempts(t *testing.T) {
	op, calls := failN(5, errTransient)
	fs := &fakeSleeper{}
	err := Do(context.Background(), Policy{}, op, WithSleeper(fs.sleep))
	if !errors.Is(err, errTransient) || len(*calls) != 1 || len(fs.delays) != 0 {
		t.Errorf("err=%v calls=%v waits=%v; want one call and no retry", err, *calls, fs.delays)
	}
}

func TestDoOnRetry(t *testing.T) {
	type call struct {
		attempt int
		delay   time.Duration
		err     error
	}
	var got []call
	op, _ := failN(2, errTransient)
	fs := &fakeSleeper{}
	err := Do(context.Background(), Policy{MaxAttempts: 5, InitialBackoff: time.Second, Rand: fixed(0.5)}, op,
		WithSleeper(fs.sleep),
		OnRetry(func(attempt int, delay time.Duration, err error) { got = append(got, call{attempt, delay, err}) }))
	if err != nil {
		t.Fatal(err)
	}
	want := []call{{1, 500 * time.Millisecond, errTransient}, {2, time.Second, errTransient}}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(call{}), cmp.Comparer(func(a, b error) bool { return a == b })); diff != "" { //nolint:errorlint // identity
		t.Errorf("OnRetry calls (-want +got):\n%s", diff)
	}
}

func TestDoCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op, calls := failN(10, errTransient)
	fs := &fakeSleeper{cancelOn: 2, cancel: cancel}
	err := Do(ctx, Policy{MaxAttempts: 5, Rand: fixed(1)}, op, WithSleeper(fs.sleep))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}
	if engine.Classify(err) != engine.KindCanceled {
		t.Errorf("kind = %v, want canceled", engine.Classify(err))
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %v, want 2", *calls)
	}
}

func TestDoCancelWithRealSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	op := func(context.Context, int) error {
		cancel() // cancel before the (one hour) wait starts
		return errTransient
	}
	start := time.Now()
	err := Do(ctx, Policy{MaxAttempts: 1, InitialBackoff: time.Hour, MaxBackoff: time.Hour, Rand: fixed(1)}, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Do did not return promptly")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := Sleep(ctx2, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on cancelled ctx = %v", err)
	}
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v", err)
	}
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("Sleep(1ms) = %v", err)
	}
}

func TestDoResetOnProgress(t *testing.T) {
	var prog Progress
	// Each attempt transfers some bytes before failing, except the last
	// ones, so the retry budget (2) only runs out after progress stops.
	attempts := 0
	op := func(context.Context, int) error {
		attempts++
		if attempts <= 5 {
			prog.Add(100)
		}
		return errTransient
	}
	fs := &fakeSleeper{}
	err := Do(context.Background(), Policy{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: time.Minute, Rand: fixed(1)}, op,
		WithSleeper(fs.sleep), ResetOnProgress(prog.Moved))
	if !errors.Is(err, errTransient) {
		t.Fatalf("Do = %v", err)
	}
	// 5 progressing failures (each resets to retry 1), then retry 2 without
	// progress, then the budget is exhausted.
	if attempts != 7 {
		t.Errorf("attempts = %d, want 7", attempts)
	}
	want := []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second, 2 * time.Second}
	if diff := cmp.Diff(want, fs.delays); diff != "" {
		t.Errorf("waits (-want +got):\n%s", diff)
	}
}

func TestProgressMoved(t *testing.T) {
	var p Progress
	if p.Moved() {
		t.Error("zero Progress reported movement")
	}
	p.Add(10)
	if !p.Moved() {
		t.Error("Add not detected")
	}
	if p.Moved() {
		t.Error("Moved must reset after being observed")
	}
}
