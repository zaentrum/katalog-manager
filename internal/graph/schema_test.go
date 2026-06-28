package graph

import (
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// TestSchemaBinds parses the SDL against the root resolver. graph-gophers
// validates every GraphQL field against a resolver method/field at parse time,
// so this fails if the schema and resolvers drift out of sync.
func TestSchemaBinds(t *testing.T) {
	r := NewResolver(nil, config.Config{}, Services{})
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("schema failed to bind to resolvers: %v", rec)
		}
	}()
	if s := MustSchema(r); s == nil {
		t.Fatal("nil schema")
	}
}
