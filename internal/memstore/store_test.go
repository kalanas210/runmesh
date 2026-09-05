package memstore_test

import (
	"testing"

	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/storetest"
)

// The external test package is what lets memstore itself stay free of any
// dependency on engine or httpapi while still proving, at compile time, that
// it satisfies both of their consumer-side interfaces.
var _ storetest.Store = (*memstore.Store)(nil)

// TestConformance is the whole point of internal/storetest: in Week 2,
// internal/pgstore runs these same four lines against a Testcontainers
// PostgreSQL, and any place where the in-memory simulation diverges from real
// SKIP LOCKED semantics fails here rather than in production.
func TestConformance(t *testing.T) {
	t.Parallel()
	storetest.RunSuite(t, func(t *testing.T) storetest.Store {
		return memstore.New(memstore.Options{})
	})
}
