package events

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// declaredTypes returns the values of the Type* constants of event.go.
func declaredTypes(t *testing.T) map[Type]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "event.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[Type]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec) //nolint:errcheck,forcetypeassert // a const declaration holds value specs
			for i, name := range vs.Names {
				if len(name.Name) < 4 || name.Name[:4] != "Type" || name.Name == "Type" {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[Type(v)] = name.Name
			}
		}
	}
	return out
}

func TestCatalogueCoversEveryEventType(t *testing.T) {
	declared := declaredTypes(t)
	if len(declared) < 10 {
		t.Fatalf("found only %d Type constants in event.go: the scan is broken", len(declared))
	}
	seen := map[Type]bool{}
	for _, k := range Catalogue() {
		if seen[k.Type] {
			t.Errorf("%s is in the catalogue twice", k.Type)
		}
		seen[k.Type] = true
		if _, ok := declared[k.Type]; !ok {
			t.Errorf("catalogue lists %q, which is not a Type constant", k.Type)
		}
		if k.Payload == nil || k.Doc == "" {
			t.Errorf("%s needs a payload type and a description", k.Type)
		}
		if _, err := json.Marshal(k.Payload); err != nil {
			t.Errorf("%s payload does not encode: %v", k.Type, err)
		}
	}
	for typ, name := range declared {
		if !seen[typ] {
			t.Errorf("%s (%q) is missing from Catalogue: add it with the type of its data", name, typ)
		}
	}
}
