package settings

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMergePatch(t *testing.T) {
	base := Settings{
		MaxConnections: Some(8),
		SpeedLimit:     Some[int64](1 << 40),
		Headers:        Some(map[string]string{"A": "1", "B": "2"}),
		Proxy:          Some(Proxy{Mode: ProxyCustom, URL: "http://proxy:8080"}),
		Retry: RetrySettings{
			MaxAttempts:    Some(3),
			InitialBackoff: Some(Duration(time.Second)),
		},
		Refresh: RefreshSettings{
			Strategies: Some([]RefreshStrategy{RefreshOriginRedirect, RefreshPageReload}),
		},
	}
	tests := []struct {
		name  string
		patch string
		want  string // JSON of the expected result
	}{
		{
			name:  "empty patch keeps everything",
			patch: `{}`,
			want:  `{"maxConnections":8,"speedLimit":1099511627776,"retry":{"maxAttempts":3,"initialBackoff":"1s"},"proxy":{"mode":"custom","url":"http://proxy:8080"},"headers":{"A":"1","B":"2"},"refresh":{"strategies":["origin-redirect","page-reload"]}}`,
		},
		{
			name:  "set and unset leaves",
			patch: `{"maxConnections":null,"userAgent":"ua","speedLimit":0}`,
			want:  `{"speedLimit":0,"retry":{"maxAttempts":3,"initialBackoff":"1s"},"proxy":{"mode":"custom","url":"http://proxy:8080"},"userAgent":"ua","headers":{"A":"1","B":"2"},"refresh":{"strategies":["origin-redirect","page-reload"]}}`,
		},
		{
			name:  "nested group merges",
			patch: `{"retry":{"maxAttempts":null,"maxBackoff":"1m0s"}}`,
			want:  `{"maxConnections":8,"speedLimit":1099511627776,"retry":{"initialBackoff":"1s","maxBackoff":"1m0s"},"proxy":{"mode":"custom","url":"http://proxy:8080"},"headers":{"A":"1","B":"2"},"refresh":{"strategies":["origin-redirect","page-reload"]}}`,
		},
		{
			name:  "null group unsets the whole group",
			patch: `{"retry":null}`,
			want:  `{"maxConnections":8,"speedLimit":1099511627776,"proxy":{"mode":"custom","url":"http://proxy:8080"},"headers":{"A":"1","B":"2"},"refresh":{"strategies":["origin-redirect","page-reload"]}}`,
		},
		{
			name:  "maps, arrays and object settings are replaced",
			patch: `{"headers":{"C":"3"},"proxy":{"mode":"none"},"refresh":{"strategies":["manual"]}}`,
			want:  `{"maxConnections":8,"speedLimit":1099511627776,"retry":{"maxAttempts":3,"initialBackoff":"1s"},"proxy":{"mode":"none"},"headers":{"C":"3"},"refresh":{"strategies":["manual"]}}`,
		},
		{
			name:  "group created from nothing",
			patch: `{"refresh":{"auto":false}}`,
			want:  `{"maxConnections":8,"speedLimit":1099511627776,"retry":{"maxAttempts":3,"initialBackoff":"1s"},"proxy":{"mode":"custom","url":"http://proxy:8080"},"headers":{"A":"1","B":"2"},"refresh":{"auto":false,"strategies":["origin-redirect","page-reload"]}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergePatch(base, []byte(tt.patch))
			if err != nil {
				t.Fatalf("MergePatch: %v", err)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var gotV, wantV any
			if err := json.Unmarshal(b, &gotV); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantV); err != nil {
				t.Fatal(err)
			}
			gb, _ := json.Marshal(gotV)
			wb, _ := json.Marshal(wantV)
			if string(gb) != string(wb) {
				t.Errorf("result\n got  %s\n want %s", gb, wb)
			}
		})
	}
	// The base must not be modified.
	if h := base.Headers.Value(); len(h) != 2 || !base.Retry.MaxAttempts.IsSet() {
		t.Errorf("base mutated: %+v", base)
	}
}

func TestMergePatchKeepsLargeIntegers(t *testing.T) {
	got, err := MergePatch(Settings{SpeedLimit: Some[int64](9007199254740993)}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if v := got.SpeedLimit.Value(); v != 9007199254740993 {
		t.Errorf("speedLimit = %d", v)
	}
}

func TestMergePatchErrors(t *testing.T) {
	for _, patch := range []string{
		``,
		`null`,
		`[]`,
		`"x"`,
		`{"maxConnections":`,
		`{} {}`,
		`{"unknownField":1}`,
		`{"retry":{"unknown":1}}`,
		`{"maxConnections":"many"}`,
		`{"retry":5}`,
	} {
		t.Run(patch, func(t *testing.T) {
			if _, err := MergePatch(Settings{}, []byte(patch)); !errors.Is(err, ErrInvalidPatch) {
				t.Errorf("err = %v, want ErrInvalidPatch", err)
			}
		})
	}
}
