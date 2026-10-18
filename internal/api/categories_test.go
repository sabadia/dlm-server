package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestCategoryOperations(t *testing.T) {
	f := newOpsFixture(t)
	var list CategoryList
	f.call(t, http.MethodGet, "/v1/categories", "", http.StatusOK, &list)
	if list.Categories == nil || len(list.Categories) != 0 {
		t.Fatalf("empty list = %+v (want an empty array, not null)", list)
	}

	var video domain.Category
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"Video","dir":"Videos","extensions":["mp4",".MKV"],"mimeTypes":["video/*"]}`, http.StatusCreated, &video)
	if video.ID == "" || video.Position != 0 || strings.Join(video.Extensions, ",") != "mp4,mkv" {
		t.Fatalf("created = %+v", video)
	}
	var docs domain.Category
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"Docs","dir":"/data/docs","extensions":["pdf"]}`, http.StatusCreated, &docs)
	f.call(t, http.MethodGet, "/v1/categories", "", http.StatusOK, &list)
	if len(list.Categories) != 2 || list.Categories[0].ID != video.ID || list.Categories[1].ID != docs.ID {
		t.Fatalf("list = %+v", list)
	}

	var got domain.Category
	f.call(t, http.MethodGet, "/v1/categories/"+docs.ID, "", http.StatusOK, &got)
	if got.Name != "Docs" || got.Dir != "/data/docs" {
		t.Errorf("get = %+v", got)
	}
	f.call(t, http.MethodPatch, "/v1/categories/"+docs.ID, `{"name":"Papers","extensions":["pdf","epub"],"position":0}`, http.StatusOK, &got)
	if got.Name != "Papers" || len(got.Extensions) != 2 || got.Position != 0 || got.Dir != "/data/docs" {
		t.Errorf("patched = %+v (the directory is unchanged)", got)
	}

	var m CategoryMatch
	f.call(t, http.MethodGet, "/v1/categories/match?name=book.EPUB", "", http.StatusOK, &m)
	if m.Category == nil || m.Category.Name != "Papers" {
		t.Errorf("match = %+v", m)
	}
	f.call(t, http.MethodGet, "/v1/categories/match?mimeType=video/webm", "", http.StatusOK, &m)
	if m.Category == nil || m.Category.Name != "Video" {
		t.Errorf("match by MIME = %+v", m)
	}
	f.call(t, http.MethodGet, "/v1/categories/match?name=data.bin", "", http.StatusOK, &m)
	if m.Category != nil {
		t.Errorf("match = %+v, want none", m)
	}

	// Errors.
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"video","dir":"x","extensions":["a"]}`, http.StatusConflict, nil)
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"Empty","dir":"x"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"Escape","dir":"../x","extensions":["a"]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/categories", `{"dir":"x","extensions":["a"]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPatch, "/v1/categories/"+docs.ID, `{"name":"Video"}`, http.StatusConflict, nil)
	f.call(t, http.MethodPatch, "/v1/categories/nope", `{"name":"X"}`, http.StatusNotFound, nil)
	f.call(t, http.MethodGet, "/v1/categories/nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodDelete, "/v1/categories/nope", "", http.StatusNotFound, nil)

	f.call(t, http.MethodDelete, "/v1/categories/"+docs.ID, "", http.StatusNoContent, nil)
	f.call(t, http.MethodGet, "/v1/categories", "", http.StatusOK, &list)
	if len(list.Categories) != 1 {
		t.Errorf("after delete: %+v", list)
	}
}

func TestTaskCreateRefusesDuplicates(t *testing.T) {
	f := newOpsFixture(t)
	body := `{"url":"` + f.fileURL() + `"}`
	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", body, http.StatusCreated, &task)
	p := decodeProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/tasks", body: body, headers: bearer(testToken)}), http.StatusConflict)
	if !strings.Contains(p.Detail, task.ID) {
		t.Errorf("detail = %q, want it to name %s", p.Detail, task.ID)
	}
	f.call(t, http.MethodPost, "/v1/tasks", `{"allowDuplicate":true,"url":"`+f.fileURL()+`"}`, http.StatusCreated, &task)
}
