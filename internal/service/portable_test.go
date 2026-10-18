package service

import (
	"context"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/portable"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestExportThenImportRoundTrips(t *testing.T) {
	ctx := t.Context()
	src := newSvcFixture(t)
	for _, name := range []string{"a.iso", "b.iso"} {
		src.create(t, CreateTaskInput{
			Source: domain.Source{
				CurrentURL: "http://127.0.0.1:1/" + name, Mirrors: []string{"http://127.0.0.2:1/" + name},
				Referrer: "http://127.0.0.1:1/", Headers: map[string]string{"Cookie": "session=SECRET"},
			},
			Name: name, Dir: src.dir,
		})
	}
	for _, format := range []string{portable.FormatJSON, portable.FormatAria2, portable.FormatURLs} {
		res, err := src.tasks.Export(ctx, format, storage.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Count != 2 || strings.Contains(res.Content, "SECRET") {
			t.Fatalf("%s: count %d, content:\n%s", format, res.Count, res.Content)
		}
		if i, j := strings.Index(res.Content, "a.iso"), strings.LastIndex(res.Content, "b.iso"); i < 0 || j < i {
			t.Errorf("%s: tasks are not oldest first:\n%s", format, res.Content)
		}

		dst := newSvcFixture(t)
		results, err := dst.tasks.Import(ctx, ImportInput{Format: format, Content: res.Content, Paused: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 2 {
			t.Fatalf("%s: %d results", format, len(results))
		}
		for _, r := range results {
			if r.Err != nil || r.Task == nil || r.Task.Status != domain.StatusPaused {
				t.Errorf("%s: result %+v", format, r)
			}
		}
		if format != portable.FormatURLs {
			if got := results[0].Task; got.Name != "a.iso" || len(got.Source.Mirrors) != 1 || got.Source.Referrer != "http://127.0.0.1:1/" {
				t.Errorf("%s: imported task = name %q mirrors %v referrer %q", format, got.Name, got.Source.Mirrors, got.Source.Referrer)
			}
		}
	}
}

func TestExportFiltersAndRejectsUnknownFormats(t *testing.T) {
	ctx := t.Context()
	f := newSvcFixture(t)
	f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/keep.bin"}})
	f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/other.bin"}})
	res, err := f.tasks.Export(ctx, portable.FormatURLs, storage.TaskFilter{Search: "keep"})
	if err != nil || res.Count != 1 || !strings.Contains(res.Content, "keep.bin") {
		t.Fatalf("filtered export = %+v, %v", res, err)
	}
	_, err = f.tasks.Export(ctx, "xml", storage.TaskFilter{})
	wantErr(t, err, domain.ErrInvalidArgument)
}

func TestExportFollowsEveryPage(t *testing.T) {
	ctx := t.Context()
	f := newSvcFixture(t)
	const n = storage.MaxTaskLimit + 5
	var in []CreateTaskInput
	for i := range n {
		in = append(in, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/f" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + itoa(i)}, AllowDuplicate: true, Paused: true})
	}
	for _, r := range f.tasks.CreateBatch(ctx, in) {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	res, err := f.tasks.Export(ctx, portable.FormatURLs, storage.TaskFilter{})
	if err != nil || res.Count != n {
		t.Fatalf("exported %d of %d tasks, %v", res.Count, n, err)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

func TestImportReportsUnreadableListsAndPerEntryFailures(t *testing.T) {
	ctx := t.Context()
	f := newSvcFixture(t)
	_, err := f.tasks.Import(ctx, ImportInput{Format: portable.FormatJSON, Content: "{"})
	wantErr(t, err, domain.ErrInvalidArgument)
	_, err = f.tasks.Import(ctx, ImportInput{Format: "xml", Content: "x"})
	wantErr(t, err, domain.ErrInvalidArgument)

	res, err := f.tasks.Import(ctx, ImportInput{Format: portable.FormatURLs, Content: "http://127.0.0.1:1/ok.bin\nnot a url\nhttp://127.0.0.1:1/ok.bin\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[0].Err != nil {
		t.Fatalf("results = %+v", res)
	}
	wantErr(t, res[1].Err, domain.ErrInvalidArgument)
	wantErr(t, res[2].Err, domain.ErrConflict) // the same link twice
}

func TestImportExpandsPatterns(t *testing.T) {
	ctx := t.Context()
	f := newSvcFixture(t)
	res, err := f.tasks.Import(ctx, ImportInput{
		Format: portable.FormatURLs, ExpandPatterns: true, Paused: true,
		Content: "http://127.0.0.1:1/img[08-11].jpg\nhttp://127.0.0.1:1/z[3-1].jpg\nhttp://127.0.0.1:1/plain.jpg\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 4 expanded tasks, one refused pattern, one plain task.
	if len(res) != 6 {
		t.Fatalf("%d results: %+v", len(res), res)
	}
	for i, want := range []string{"img08.jpg", "img09.jpg", "img10.jpg", "img11.jpg"} {
		if res[i].Err != nil || !strings.HasSuffix(res[i].Task.Source.CurrentURL, want) || res[i].Task.Source.OriginalURL != res[i].Task.Source.CurrentURL {
			t.Errorf("result %d = %+v", i, res[i])
		}
	}
	wantErr(t, res[4].Err, domain.ErrInvalidArgument)
	if res[5].Err != nil {
		t.Errorf("plain: %v", res[5].Err)
	}
}

func TestExpandPatternInputRules(t *testing.T) {
	named := CreateTaskInput{Source: domain.Source{CurrentURL: "http://x/f[1-3]"}, Name: "f.bin"}
	if _, err := ExpandPatternInput(named); err == nil {
		t.Error("a name with a pattern of several URLs was accepted")
	}
	one := CreateTaskInput{Source: domain.Source{CurrentURL: "http://x/f[1-1]"}, Name: "f.bin"}
	if got, err := ExpandPatternInput(one); err != nil || len(got) != 1 || got[0].Source.CurrentURL != "http://x/f1" {
		t.Errorf("single expansion = %+v, %v", got, err)
	}
	resolved := CreateTaskInput{ResolveID: "r1"}
	if got, err := ExpandPatternInput(resolved); err != nil || len(got) != 1 {
		t.Errorf("resolve id input = %+v, %v", got, err)
	}
	plain := CreateTaskInput{Source: domain.Source{CurrentURL: "http://[::1]:80/f", OriginalURL: "http://orig/"}}
	if got, _ := ExpandPatternInput(plain); got[0].Source.OriginalURL != "http://orig/" {
		t.Errorf("an unexpanded input lost its original URL: %+v", got[0].Source)
	}
}

func TestStats(t *testing.T) {
	ctx := t.Context()
	f := newSvcFixture(t)
	done := f.withStatus(t, domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted)
	f.withStatus(t, domain.StatusRunning, domain.StatusPaused)
	f.withStatus(t)
	task := f.get(t, done.ID)
	task.Downloaded, task.Size = 1000, 1000
	if err := f.store.Tasks().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	st, err := f.tasks.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tasks != 3 || st.ByStatus[domain.StatusCompleted] != 1 || st.ByStatus[domain.StatusPaused] != 1 || st.ByStatus[domain.StatusQueued] != 1 {
		t.Errorf("stats = %+v", st)
	}
	if st.DownloadedBytes != 1000 {
		t.Errorf("downloaded = %d", st.DownloadedBytes)
	}
	if len(st.ByEngine) == 0 {
		t.Errorf("no engine rows: %+v", st)
	}
	if len(st.Completed) != 3 || st.Completed[0].Period != "24h" || st.Completed[0].Tasks != 1 || st.Completed[0].Bytes != 1000 || st.Completed[2].Tasks != 1 {
		t.Errorf("completed = %+v", st.Completed)
	}
}
