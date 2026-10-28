package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The checked-in reference documents are generated: `make docs` rewrites them.
// These tests fail when a command, flag or API operation changed and the
// documents were not regenerated.

func TestCLIReferenceIsUpToDate(t *testing.T) {
	var got bytes.Buffer
	if err := writeReference(&got, NewRootCommand("dev")); err != nil {
		t.Fatal(err)
	}
	checkGenerated(t, filepath.Join("..", "..", "docs", "CLI.md"), got.Bytes())
}

func TestOpenAPIDocumentIsUpToDate(t *testing.T) {
	// make docs runs `go run`, whose version is "dev".
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), "dev", []string{"openapi"}, WithIO(&stdout, &stderr))
	if code != ExitOK {
		t.Fatalf("dlm openapi: exit %d: %s", code, stderr.String())
	}
	checkGenerated(t, filepath.Join("..", "..", "docs", "openapi.json"), stdout.Bytes())
}

func TestOpenAPI30DocumentIsUpToDate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), "dev", []string{"openapi", "--oas", "3.0"}, WithIO(&stdout, &stderr))
	if code != ExitOK {
		t.Fatalf("dlm openapi --oas 3.0: exit %d: %s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"openapi": "3.0.3"`)) {
		t.Errorf("not an OpenAPI 3.0.3 document: %.200s", stdout.String())
	}
	checkGenerated(t, filepath.Join("..", "..", "docs", "openapi-3.0.json"), stdout.Bytes())
}

func TestOpenAPIRejectsUnknownVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), "dev", []string{"openapi", "--oas", "2.0"}, WithIO(&stdout, &stderr)); code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}

func checkGenerated(t *testing.T, path string, want []byte) {
	t.Helper()
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (generate it with `make docs`)", err)
	}
	// Git may check files out with CRLF on Windows.
	have = bytes.ReplaceAll(have, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(have, want) {
		t.Errorf("%s is out of date: run `make docs` and commit the result", path)
	}
}
