package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	graphql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/rest"
)

// routes mounts the authenticated surface on r, behind authn: GraphQL, whose
// root fields refuse whom they are not for (graph/access.go); the console's
// live catalog stream, for admins; and the REST routes, each for whom it is
// (rest.Register).
func routes(r chi.Router, authn func(http.Handler) http.Handler, pol auth.Policy, schema *graphql.Schema,
	stream http.HandlerFunc, api *rest.Handlers) {
	r.Group(func(pr chi.Router) {
		pr.Use(authn)
		gqlHandler := &relay.Handler{Schema: schema}
		pr.Handle("/query", gqlHandler)
		pr.Handle("/graphql", gqlHandler)
		// Also serve GraphQL under the /api/manage prefix so it resolves behind
		// the demo's path-routing (the portal's katalog console posts there).
		pr.Handle("/api/manage/query", gqlHandler)
		pr.Handle("/api/manage/graphql", gqlHandler)
		// What the pipeline does to which title, as it happens: the console's.
		pr.With(pol.Require(auth.Admin)).Get("/api/manage/stream", stream)
		api.Register(pr)
	})
}
