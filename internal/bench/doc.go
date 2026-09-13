// Package bench holds RunMesh's benchmarks, and it is the one place in this
// repository where a test does not live beside the code it exercises.
//
// WHY THEY ARE TOGETHER. Almost none of these numbers mean anything alone. The
// question is never "how fast is Claim" — it is "how much does durability
// cost", "does a ninth worker buy anything", "is the SKIP LOCKED queue still
// linear at thirty-two claimers". Every one of those is a COMPARISON, and a
// comparison whose two halves live in two packages is a comparison nobody ever
// runs as a pair: one half gets re-measured after a change and the other keeps
// its number from three weeks ago. Putting them in one package means one `go
// test -bench` invocation produces both sides of every claim the README wants
// to make, on one machine, in one process, in one minute.
//
// It is also the only arrangement the import graph permits. internal/pgstore
// has no business importing internal/engine, internal/engine has no business
// importing internal/pgstore, and the scheduler benchmark needs both — the
// whole point of it is that the same dispatcher, the same worker pool and the
// same Classify run over two completely different stores. A third package that
// imports both and that NOTHING imports back is where that measurement can
// exist at all. The same argument produced internal/storetest in Week 2, and
// this package is its benchmark-shaped sibling: storetest asserts that the two
// stores AGREE, this one measures what they cost.
//
// WHY IT IS UNDER internal/. So that internal/clock/purity_test.go walks it.
// A benchmark is exactly the kind of file that wants to call time.Now twice and
// subtract, and a benchmark exempt from the one rule the rest of the suite
// lives under is how that rule starts eroding. It does not need the exemption:
// testing.B.Elapsed() reports the timed interval with the stopped stretches
// already removed, which is more correct than a hand-rolled subtraction anyway,
// because the expensive setup these benchmarks do would otherwise land inside
// the rate.
//
// WHAT THE NUMBERS ARE NOT. Every one of them is a single process on one
// developer machine against a PostgreSQL container running with fsync=off.
// That is the right configuration for measuring THIS code — it removes the
// storage device from the comparison so that what is left is the query plan,
// the lock behaviour and the Go — and it is the wrong configuration for
// predicting production, where a commit is a disk write. docs/benchmarks
// spells out which claims survive that gap and which do not.
package bench
