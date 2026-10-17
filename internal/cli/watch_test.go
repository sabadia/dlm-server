package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/events"
)

// syncBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startWatch runs `dlm watch` with args in the background against a real
// server streaming bus. It returns the output buffers and a function that
// cancels the command and returns its exit code.
func startWatch(t *testing.T, bus *events.Bus, args ...string) (stdout, stderr *syncBuffer, stop func() int) {
	t.Helper()
	base := startAPIWith(t, api.Deps{Token: testToken, Version: testSrvVersion, Bus: bus})
	stdout, stderr = &syncBuffer{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	code := make(chan int, 1)
	home := t.TempDir()
	go func() {
		code <- run(ctx, testCLIVersion, append([]string{"--server", base, "--token", testToken, "watch"}, args...),
			WithIO(stdout, stderr),
			WithEnviron(func() []string { return []string{"DLM_HOME=" + home} }),
		)
	}()
	var once sync.Once
	var exit int
	stop = func() int {
		once.Do(func() {
			cancel()
			select {
			case exit = <-code:
			case <-time.After(10 * time.Second):
				t.Fatal("watch did not exit after cancel")
			}
		})
		return exit
	}
	t.Cleanup(func() { stop() })
	return stdout, stderr, stop
}

// waitConnected publishes probe events until the watcher prints one, which
// proves its subscription is live.
func waitConnected(t *testing.T, bus *events.Bus, out *syncBuffer, probe events.Event, seen func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !seen(out.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("watch printed nothing; stdout: %q", out.String())
		}
		bus.Publish(probe)
		time.Sleep(5 * time.Millisecond)
	}
}

func waitFor(t *testing.T, out *syncBuffer, cond func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond(out.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out; stdout: %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchJSON(t *testing.T) {
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	stdout, stderr, stop := startWatch(t, bus, "-o", "json", "--type", "task.status,task.created", "--task", "t1")

	waitConnected(t, bus, stdout, events.Event{Type: events.TypeTaskCreated, TaskID: "t1"},
		func(s string) bool { return strings.Contains(s, "\n") })
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t2"})  // filtered out
	bus.Publish(events.Event{Type: events.TypeTaskUpdated, TaskID: "t1"}) // filtered out
	bus.Publish(events.Event{
		Type: events.TypeTaskStatus, TaskID: "t1", QueueID: "q1",
		Data: map[string]string{"from": "queued", "to": "running"},
	})
	waitFor(t, stdout, func(s string) bool { return strings.Contains(s, `"type":"task.status"`) })

	if code := stop(); code != ExitOK {
		t.Fatalf("exit %d, want 0 on cancel; stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var last client.Event
	for i, line := range lines {
		var ev client.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not a JSON object: %q", i, line)
		}
		if ev.TaskID != "t1" || (ev.Type != "task.created" && ev.Type != "task.status") {
			t.Fatalf("line %d: unexpected event %+v", i, ev)
		}
		if !strings.HasSuffix(ev.EventID, "-"+strconv.FormatUint(ev.ID, 10)) {
			t.Fatalf("line %d: eventId %q does not end with seq %d", i, ev.EventID, ev.ID)
		}
		last = ev
	}
	if last.Type != "task.status" || last.QueueID != "q1" || string(last.Data) != `{"from":"queued","to":"running"}` {
		t.Fatalf("last event = %+v", last)
	}
}

func TestWatchTable(t *testing.T) {
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	stdout, _, stop := startWatch(t, bus)

	waitConnected(t, bus, stdout, events.Event{Type: events.TypeQueueUpdated, QueueID: "q9"},
		func(s string) bool { return strings.Contains(s, "queue.updated") })
	bus.Publish(events.Event{
		Type: events.TypeTaskStatus, TaskID: "t1",
		Data: map[string]any{"from": "running", "to": "error", "error": map[string]any{"message": "disk full"}},
	})
	waitFor(t, stdout, func(s string) bool { return strings.Contains(s, "running -> error: disk full") })
	if code := stop(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(stdout.String(), "\n")
	if got := strings.Join(strings.Fields(lines[0]), " "); got != "TIME TYPE TASK QUEUE SUMMARY" {
		t.Fatalf("header = %q", lines[0])
	}
	if f := strings.Fields(lines[1]); len(f) < 4 || f[1] != "queue.updated" || f[2] != "-" || f[3] != "q9" {
		t.Fatalf("row = %q", lines[1])
	}
}

func TestWatchUnauthorizedFails(t *testing.T) {
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	base := startAPIWith(t, api.Deps{Token: testToken, Version: testSrvVersion, Bus: bus})
	r := runCLI(t.Context(), t, "", "--server", base, "--token", "wrong", "watch")
	if r.code != ExitError || !strings.Contains(r.stderr, "Unauthorized") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestEventSummary(t *testing.T) {
	tests := []struct {
		typ, data, want string
	}{
		{"task.progress", `{"downloaded":1572864,"size":3145728,"speed":1024}`, "1.5 MiB / 3.0 MiB (50%) 1.0 KiB/s"},
		{"task.progress", `{"downloaded":512,"size":0,"speed":0}`, "512 B"},
		{"task.status", `{"from":"queued","to":"running"}`, "queued -> running"},
		{"queue.updated", `{"name":"q"}`, `{"name":"q"}`},
		{"task.deleted", ``, ""},
		{"settings.updated", `"` + strings.Repeat("é", 100) + `"`, `"` + strings.Repeat("é", 76) + "..."},
	}
	for _, tt := range tests {
		ev := client.Event{Type: tt.typ, Data: json.RawMessage(tt.data)}
		if got := eventSummary(ev); got != tt.want {
			t.Errorf("eventSummary(%s %s) = %q, want %q", tt.typ, tt.data, got, tt.want)
		}
	}
}
