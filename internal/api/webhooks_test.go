package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestWebhookOperations(t *testing.T) {
	f := newOpsFixture(t)
	var list WebhookList
	f.call(t, http.MethodGet, "/v1/webhooks", "", http.StatusOK, &list)
	if list.Webhooks == nil || len(list.Webhooks) != 0 {
		t.Fatalf("empty list = %+v (want an empty array, not null)", list)
	}

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(receiver.Close)
	var hook domain.Webhook
	body := `{"url":"` + receiver.URL + `/hook","events":["task.status","queue.completed"],"secret":"s3cret"}`
	raw := do(t, f.s, request{method: http.MethodPost, path: "/v1/webhooks", body: body, headers: bearer(testToken)})
	if raw.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", raw.Code, raw.Body)
	}
	if strings.Contains(raw.Body.String(), "s3cret") || !strings.Contains(raw.Body.String(), `"hasSecret":true`) {
		t.Errorf("the response must say a secret exists without returning it: %s", raw.Body)
	}
	f.call(t, http.MethodPost, "/v1/webhooks", `{"url":"http://localhost:9/h","events":["task.created"],"disabled":true}`, http.StatusCreated, &hook)
	if hook.Enabled || hook.HasSecret {
		t.Errorf("created = %+v", hook)
	}

	f.call(t, http.MethodGet, "/v1/webhooks", "", http.StatusOK, &list)
	if len(list.Webhooks) != 2 {
		t.Fatalf("list = %+v", list)
	}
	id := list.Webhooks[0].ID
	var got domain.Webhook
	f.call(t, http.MethodGet, "/v1/webhooks/"+id, "", http.StatusOK, &got)
	if len(got.Events) != 2 || !got.HasSecret {
		t.Errorf("get = %+v", got)
	}
	f.call(t, http.MethodPatch, "/v1/webhooks/"+id, `{"events":["task.deleted"],"enabled":false,"secret":""}`, http.StatusOK, &got)
	if len(got.Events) != 1 || got.Enabled || got.HasSecret {
		t.Errorf("patched = %+v", got)
	}

	// The test delivery goes out even though the webhook is disabled.
	f.call(t, http.MethodPost, "/v1/webhooks/"+id+"/test", "", http.StatusNoContent, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f.call(t, http.MethodPatch, "/v1/webhooks/"+id, `{"url":"`+deadURL+`"}`, http.StatusOK, nil)
	f.call(t, http.MethodPost, "/v1/webhooks/"+id+"/test", "", http.StatusBadGateway, nil)

	// Errors.
	f.call(t, http.MethodPost, "/v1/webhooks", `{"url":"ftp://x.example/","events":["task.status"]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/webhooks", `{"url":"https://x.example/","events":["task.progress"]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/webhooks", `{"url":"https://x.example/","events":[]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPatch, "/v1/webhooks/"+id, `{"url":"mailto:x@y"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodGet, "/v1/webhooks/nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodPatch, "/v1/webhooks/nope", `{"enabled":true}`, http.StatusNotFound, nil)
	f.call(t, http.MethodPost, "/v1/webhooks/nope/test", "", http.StatusNotFound, nil)
	f.call(t, http.MethodDelete, "/v1/webhooks/nope", "", http.StatusNotFound, nil)

	f.call(t, http.MethodDelete, "/v1/webhooks/"+id, "", http.StatusNoContent, nil)
	f.call(t, http.MethodGet, "/v1/webhooks", "", http.StatusOK, &list)
	if len(list.Webhooks) != 1 {
		t.Errorf("after delete: %+v", list)
	}
}

func TestWebhookDeliveriesAndPluginLogs(t *testing.T) {
	f := newOpsFixture(t)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(receiver.Close)
	var list WebhookList
	f.call(t, http.MethodPost, "/v1/webhooks", `{"url":"`+receiver.URL+`/hook","events":["task.status"]}`, http.StatusCreated, nil)
	f.call(t, http.MethodGet, "/v1/webhooks", "", http.StatusOK, &list)
	id := list.Webhooks[0].ID

	var d WebhookDeliveries
	f.call(t, http.MethodGet, "/v1/webhooks/"+id+"/deliveries", "", http.StatusOK, &d)
	if d.Deliveries == nil || len(d.Deliveries) != 0 {
		t.Errorf("no deliveries yet must be []: %+v", d.Deliveries)
	}
	f.call(t, http.MethodPost, "/v1/webhooks/"+id+"/test", "", http.StatusNoContent, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f.call(t, http.MethodPatch, "/v1/webhooks/"+id, `{"url":"`+deadURL+`"}`, http.StatusOK, nil)
	f.call(t, http.MethodPost, "/v1/webhooks/"+id+"/test", "", http.StatusBadGateway, nil)

	f.call(t, http.MethodGet, "/v1/webhooks/"+id+"/deliveries", "", http.StatusOK, &d)
	if len(d.Deliveries) != 2 {
		t.Fatalf("deliveries = %+v", d.Deliveries)
	}
	if bad, good := d.Deliveries[0], d.Deliveries[1]; bad.OK || bad.Status != 0 || bad.Error == "" || !good.OK || good.Status != http.StatusNoContent || good.Event != "webhook.test" {
		t.Errorf("newest first: %+v", d.Deliveries)
	}
	f.call(t, http.MethodGet, "/v1/webhooks/nope/deliveries", "", http.StatusNotFound, nil)

	// A plugin's log: its dlm.log calls at load time.
	src := pluginDir(t, "talker", `dlm.log.info("hello from the plugin", 7); `+goodScript)
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":`+jsonString(src)+`}`, http.StatusCreated, nil)
	var l LogList
	f.call(t, http.MethodGet, "/v1/plugins/talker/logs", "", http.StatusOK, &l)
	if len(l.Records) == 0 || l.Records[0].Message != "hello from the plugin 7" || l.Records[0].Attrs["plugin"] != "talker" || l.BootID == "" {
		t.Errorf("plugin log = %+v", l)
	}
	f.call(t, http.MethodGet, "/v1/plugins/talker/logs?level=error", "", http.StatusOK, &l)
	if l.Records == nil || len(l.Records) != 0 {
		t.Errorf("level filter: %+v", l.Records)
	}
	f.call(t, http.MethodGet, "/v1/plugins/nope/logs", "", http.StatusNotFound, nil)
	f.call(t, http.MethodGet, "/v1/plugins/talker/logs?level=loud", "", http.StatusUnprocessableEntity, nil)
}
