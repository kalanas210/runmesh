// Package integration holds RunMesh's failure tests: the cases that assert what
// happens when something breaks rather than when everything works.
//
// WHY THEY ARE NOT BESIDE A PACKAGE. Every other test in this repository lives
// next to the code it exercises, and that is right, because every other test has
// a subject. These do not. "A worker dies and the reconciler recovers the step"
// is a claim about the engine AND the store AND the transition policy AND the
// API that reports the outcome, and there is no package it belongs to — putting
// it in internal/engine would make it a test of a mock, and putting it in
// internal/memstore would make it a test of a store with no runtime attached.
// So they live here, one directory up from both, and they wire the real pieces
// together.
//
// WHAT THEY ARE NOT. They are not cmd/server/crash_test.go, which kills a real
// operating-system process with SIGKILL and requires a second process against
// the same PostgreSQL to finish the job. That test covers the half of recovery
// that only a real crash can produce, and it is the right test for it. These
// cover the half that a crash test cannot reach cheaply: the exact state of a
// step after a lease expiry, the retry budget arithmetic, what a cancellation
// does to the steps downstream of the one that was running, and where admission
// control starts saying no. They run in-process, in seconds rather than in
// minutes, and the in-memory half of every case needs no database at all —
// which is the difference between a failure test somebody runs and one they
// mean to.
//
// WHY THEY ARE BEHIND A BUILD TAG. `make test-integration` runs them with
// -tags=integration, and plain `go test ./...` does not compile them at all.
// They are fast — a couple of seconds for the whole file — but they bind a real
// listener, start real goroutines and wait out real lease deadlines, and the
// unit suite is a loop somebody runs between edits. The tag is also what lets
// the PostgreSQL half of every case be unconditional rather than skipped: the
// target that runs them starts a database first.
//
// HOW THEY SIMULATE A DEAD WORKER. Not by killing anything. A worker that died
// leaves exactly one thing behind — a lease, held by an owner that will never
// heartbeat it again — so these tests produce that state directly: claim a step
// as an owner that does not exist, Start it, and then never touch it. The store
// cannot tell the difference, because there is no difference; the whole point of
// a lease is that liveness is expressed by renewal and by nothing else. Doing it
// this way also makes the test deterministic, where killing a goroutine
// mid-flight would not be: the engine's drain RELEASES leases on the way out, so
// a politely stopped worker is the opposite of the situation under test.
package integration
