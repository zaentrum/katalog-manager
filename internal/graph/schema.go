// Package graph implements the GraphQL surface (graph-gophers/graphql-go,
// codegen-free: resolvers are plain Go methods bound by reflection at
// schema-parse time). The SDL lives in schema.graphql.
package graph

import (
	_ "embed"
	"strconv"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
)

//go:embed schema.graphql
var sdl string

// MustSchema parses the SDL against the root resolver, panicking on any
// schema↔resolver mismatch (caught at startup and in tests).
func MustSchema(root *Resolver) *graphql.Schema {
	opts := []graphql.SchemaOpt{
		graphql.MaxParallelism(8),
		graphql.UseStringDescriptions(),
	}
	return graphql.MustParseSchema(sdl, root, opts...)
}

// SDL returns the raw schema text (used by tests).
func SDL() string { return sdl }

// ---- conversion helpers (model storage types -> GraphQL types) ----

func gid(s string) graphql.ID { return graphql.ID(s) }

func gtime(t *time.Time) *graphql.Time {
	if t == nil {
		return nil
	}
	return &graphql.Time{Time: *t}
}

// i64ptrToFloat maps a nullable int64 column to a GraphQL Float (which is the
// only JSON-number type wide enough for ms/bytes without precision loss in 32-bit Int).
func i64ptrToFloat(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func i64ToFloat(v int64) float64 { return float64(v) }

func i64ToFloatNonNil(v *int64) float64 {
	if v == nil {
		return 0
	}
	return float64(*v)
}

// derefBool returns the pointed bool or false.
func derefBool(b *bool) bool {
	return b != nil && *b
}

func itoa32(v int32) string { return strconv.FormatInt(int64(v), 10) }
