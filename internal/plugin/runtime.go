package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Sandbox limits.
const (
	maxScriptBytes   = 1 << 20
	maxCallStack     = 256
	maxHTTPBody      = 8 << 20
	maxRedirects     = 5
	defaultHTTPLimit = 20 * time.Second
	maxErrorText     = 500
)

// ErrNoHandler means the plugin registered no function for a hook.
var ErrNoHandler = errors.New("plugin: no handler registered for the hook")

// Env is what an Instance needs from the daemon.
type Env struct {
	Logger *slog.Logger
	// HTTP sends the requests of dlm.http. It should carry no cookie jar and
	// no stored logins: a plugin only gets what it asks for itself. Nil
	// disables dlm.http.
	HTTP *http.Client
	// StorageFile holds dlm.storage; empty disables it.
	StorageFile string
}

// Instance is one loaded plugin: a JavaScript runtime with the plugin's
// script run and its handlers registered. Its methods are safe for concurrent
// use; calls into one plugin never overlap.
type Instance struct {
	m       *Manifest
	log     *slog.Logger
	timeout time.Duration
	httpc   *http.Client
	kv      *kvStore

	mu       sync.Mutex // guards vm, handlers and callCtx
	vm       *goja.Runtime
	handlers map[string]goja.Callable
	callCtx  context.Context
}

// Load runs the plugin's script (ctx ends it early) and returns the instance with its handlers
// registered. The script is run under the same timeout as a hook call.
func Load(ctx context.Context, m *Manifest, script []byte, env Env) (*Instance, error) {
	if len(script) > maxScriptBytes {
		return nil, fmt.Errorf("plugin %s: the script is larger than %d bytes", m.Name, maxScriptBytes)
	}
	timeout, err := m.CallTimeout()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Name, err)
	}
	log := env.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	i := &Instance{
		m: m, log: log.With("plugin", m.Name), timeout: timeout, vm: goja.New(),
		handlers: make(map[string]goja.Callable), callCtx: context.Background(),
	}
	if env.StorageFile != "" {
		i.kv = newKV(env.StorageFile)
	}
	if env.HTTP != nil && len(m.Net) > 0 {
		c := *env.HTTP
		c.Jar = nil
		c.Timeout = 0 // each request has its own deadline
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if !m.AllowsHost(req.URL.Hostname()) {
				return fmt.Errorf("redirect to %s, which the plugin may not contact", req.URL.Hostname())
			}
			return nil
		}
		i.httpc = &c
	}
	i.vm.SetMaxCallStackSize(maxCallStack)
	i.install() //nolint:contextcheck // goja callbacks carry no context: the running call's is kept on the instance

	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.run(ctx, func() error {
		_, err := i.vm.RunScript(m.Main, string(script))
		return err //nolint:wrapcheck // converted by run
	}); err != nil {
		return nil, err
	}
	return i, nil
}

// Manifest returns the plugin's manifest.
func (i *Instance) Manifest() *Manifest { return i.m }

// Handles reports whether the plugin registered a function for hook.
func (i *Instance) Handles(hook string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.handlers[hook]
	return ok
}

// Call runs the plugin's function for hook with arg (a JSON-like value) and
// returns what it returned: nil, a bool, a number, a string, a []any or a
// map[string]any. It returns ErrNoHandler when the plugin has no function for
// the hook, and an error when the function throws, runs past the timeout, or
// ctx ends.
func (i *Instance) Call(ctx context.Context, hook string, arg any) (any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	fn, ok := i.handlers[hook]
	if !ok {
		return nil, ErrNoHandler
	}
	var out any
	err := i.run(ctx, func() error {
		v, err := fn(goja.Undefined(), i.vm.ToValue(arg))
		if err != nil {
			return err //nolint:wrapcheck // converted by run
		}
		out, err = i.settle(v)
		return err
	})
	return out, err
}

// settle exports a handler's return value; an async handler's promise must
// already be settled (goja runs its jobs before the call returns).
func (i *Instance) settle(v goja.Value) (any, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil //nolint:nilnil // no result is a result
	}
	if p, ok := v.Export().(*goja.Promise); ok {
		switch p.State() {
		case goja.PromiseStateFulfilled:
			return i.settle(p.Result())
		case goja.PromiseStateRejected:
			return nil, fmt.Errorf("the handler rejected: %s", clip(p.Result().String()))
		default:
			return nil, errors.New("the handler returned a promise that is still pending; only synchronous work is supported")
		}
	}
	return v.Export(), nil
}

// run executes fn with the instance locked, interrupting the script when the
// timeout passes or ctx ends, and turns goja's errors into plain ones.
func (i *Instance) run(ctx context.Context, fn func() error) error {
	i.callCtx = ctx
	var (
		mu       sync.Mutex
		finished bool
		reason   string
	)
	interrupt := func(why string) {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			reason = why
			i.vm.Interrupt(why)
		}
	}
	timer := time.AfterFunc(i.timeout, func() { interrupt("timeout") })
	stop := context.AfterFunc(ctx, func() { interrupt("canceled") })
	err := fn()
	timer.Stop()
	stop()
	mu.Lock()
	finished = true
	why := reason
	mu.Unlock()
	i.vm.ClearInterrupt()
	i.callCtx = context.Background()

	var ie *goja.InterruptedError
	var ex *goja.Exception
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ie):
		if why == "canceled" {
			return fmt.Errorf("plugin %s: the call was canceled: %w", i.m.Name, context.Canceled)
		}
		return fmt.Errorf("plugin %s: the script ran longer than %s and was stopped", i.m.Name, i.timeout)
	case errors.As(err, &ex):
		return fmt.Errorf("plugin %s: %s", i.m.Name, clip(ex.Error()))
	default:
		return fmt.Errorf("plugin %s: %w", i.m.Name, err)
	}
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorText {
		return s[:maxErrorText] + "…"
	}
	return s
}

// install defines the dlm global.
func (i *Instance) install() {
	vm := i.vm
	dlm := vm.NewObject()
	_ = dlm.Set("on", func(call goja.FunctionCall) goja.Value {
		hook := call.Argument(0).String()
		fn, ok := goja.AssertFunction(call.Argument(1))
		if _, declared := i.m.Hooks[hook]; !declared {
			panic(vm.NewTypeError("dlm.on: hook %q is not declared in the manifest", hook))
		}
		if !ok {
			panic(vm.NewTypeError("dlm.on: the second argument must be a function"))
		}
		i.handlers[hook] = fn
		return goja.Undefined()
	})
	_ = dlm.Set("plugin", map[string]any{"name": i.m.Name, "version": i.m.Version})

	logObj := vm.NewObject()
	for name, level := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		_ = logObj.Set(name, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for n, a := range call.Arguments {
				parts[n] = fmt.Sprint(a.Export())
			}
			i.log.Log(i.callCtx, level, clip(strings.Join(parts, " ")))
			return goja.Undefined()
		})
	}
	_ = dlm.Set("log", logObj)

	httpObj := vm.NewObject()
	_ = httpObj.Set("get", func(call goja.FunctionCall) goja.Value {
		return i.fetch(http.MethodGet, call.Argument(0), goja.Undefined(), call.Argument(1))
	})
	_ = httpObj.Set("post", func(call goja.FunctionCall) goja.Value {
		return i.fetch(http.MethodPost, call.Argument(0), call.Argument(1), call.Argument(2))
	})
	_ = dlm.Set("http", httpObj)

	st := vm.NewObject()
	_ = st.Set("get", func(call goja.FunctionCall) goja.Value {
		v, ok, err := i.storage().get(call.Argument(0).String())
		if err != nil {
			panic(vm.NewGoError(err))
		}
		if !ok {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})
	_ = st.Set("set", func(call goja.FunctionCall) goja.Value {
		if err := i.storage().set(call.Argument(0).String(), call.Argument(1).Export()); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})
	_ = st.Set("delete", func(call goja.FunctionCall) goja.Value {
		if err := i.storage().delete(call.Argument(0).String()); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})
	_ = st.Set("keys", func(goja.FunctionCall) goja.Value {
		keys, err := i.storage().keys()
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(keys)
	})
	_ = dlm.Set("storage", st)
	_ = vm.Set("dlm", dlm)
}

func (i *Instance) storage() *kvStore {
	if i.kv == nil {
		panic(i.vm.NewTypeError("dlm.storage is not available"))
	}
	return i.kv
}

// fetch implements dlm.http.get and post.
func (i *Instance) fetch(method string, rawURL, body, opts goja.Value) goja.Value {
	vm := i.vm
	if i.httpc == nil {
		panic(vm.NewTypeError("dlm.http is not available: list the hosts the plugin may contact in the manifest's net"))
	}
	u, err := url.Parse(rawURL.String())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		panic(vm.NewTypeError("dlm.http: %q is not an http(s) URL", rawURL.String()))
	}
	if !i.m.AllowsHost(u.Hostname()) {
		panic(vm.NewTypeError("dlm.http: the plugin may not contact %s; add it to the manifest's net", u.Hostname()))
	}
	headers := http.Header{}
	timeout := defaultHTTPLimit
	if o, ok := opts.Export().(map[string]any); ok {
		if h, ok := o["headers"].(map[string]any); ok {
			for k, v := range h {
				headers.Set(k, fmt.Sprint(v))
			}
		}
		if ms, ok := o["timeout"].(int64); ok && ms > 0 {
			timeout = min(time.Duration(ms)*time.Millisecond, i.timeout)
		}
	}
	var reader io.Reader
	switch b := body.Export().(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			panic(vm.NewTypeError("dlm.http: the body cannot be encoded as JSON: %v", err))
		}
		reader = bytes.NewReader(data)
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
	}
	ctx, cancel := context.WithTimeout(i.callCtx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		panic(vm.NewGoError(err))
	}
	req.Header = headers
	resp, err := i.httpc.Do(req)
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("dlm.http: %w", err)))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("dlm.http: read response: %w", err)))
	}
	if len(data) > maxHTTPBody {
		panic(vm.NewGoError(fmt.Errorf("dlm.http: the response is larger than %d bytes", maxHTTPBody)))
	}
	hs := make(map[string]any, len(resp.Header))
	for k, v := range resp.Header {
		hs[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	res := vm.NewObject()
	_ = res.Set("status", resp.StatusCode)
	_ = res.Set("headers", hs)
	_ = res.Set("body", string(data))
	_ = res.Set("json", func(goja.FunctionCall) goja.Value {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			panic(vm.NewGoError(fmt.Errorf("dlm.http: the response is not JSON: %w", err)))
		}
		return vm.ToValue(v)
	})
	return res
}
