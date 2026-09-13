package memstore_test

import (
	"testing"

	"github.com/kalanas210/runmesh/internal/eventbus"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/storetest"
)

// The external test package is what lets memstore itself stay free of any
// dependency on engine or httpapi while still proving, at compile time, that
// it satisfies both of their consumer-side interfaces.
var _ storetest.Store = (*memstore.Store)(nil)

// And this is the live-event consumer subscribe.go names: internal/eventbus,
// which fans this store's stream out to the Server-Sent Events handler. The
// assertion is here rather than in a comment alone because the doc comment on
// `subscriber` once named a WebSocket fan-out that ADR 0013 had already
// rejected, and a claim about who consumes a channel is worth a line the
// compiler checks.
var _ eventbus.Source = (*memstore.Store)(nil)

// TestConformance is the whole point of internal/storetest: internal/pgstore
// runs these same four lines against a real PostgreSQL, and any place where the
// in-memory simulation diverges from real SKIP LOCKED semantics fails in CI
// rather than in production.
func TestConformance(t *testing.T) {
	t.Parallel()
	storetest.RunSuite(t, func(t *testing.T) storetest.Store {
		return memstore.New(memstore.Options{})
	})
}
