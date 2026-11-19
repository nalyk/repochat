package tools

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/read_catalogue.json from New(r)")

const readCatalogueGolden = "testdata/read_catalogue.json"

// catalogueJSON is every tool as tools/list advertises it: name, title,
// description, input schema and annotations.
func catalogueJSON(t *testing.T, list []mcp.Tool) []byte {
	t.Helper()
	defs := make([]map[string]any, len(list))
	for i, tool := range list {
		defs[i] = mcp.ToolJSON(tool)
	}
	raw, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// The golden was captured from New(r) as it was on main, before write mode
// touched tools.go. Read mode must advertise exactly the same catalogue.
func TestReadModeCatalogueIsUnchanged(t *testing.T) {
	_, r, _ := fixture(t, repo.Options{})
	got := catalogueJSON(t, New(r))
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(readCatalogueGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(readCatalogueGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(readCatalogueGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the read-mode catalogue changed:\n%s", got)
	}
}
