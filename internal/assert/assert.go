//go:build assert

// Package assert provides runtime precondition checks.
//
// Power of 10 rule 5 asks for at least two assertions per function. Go has no
// assert, and unconditional runtime checks are not free on a Pi Zero 2 W, so the
// checks compile to nothing unless the binary is built with -tags assert. Tests
// run both ways: `make test` without, `make test-assert` with.
//
// Assertions state invariants the code believes are already true. They are not
// a substitute for validating untrusted input, which must always return an
// error regardless of build tags.
//
// # Why there are two functions
//
// Rule 5 collides with rule 3 (no allocation after initialisation). Passing a
// value to a ...any parameter boxes it on the heap, and that happens when the
// arguments are evaluated — before the function is entered, and therefore even
// when the assertion passes. A formatted assertion in a per-block code path
// allocates on every call.
//
// So: That takes a constant message and never allocates, for hot paths. Thatf
// formats, for constructors and other code that runs once.
package assert

import "fmt"

// Enabled reports whether assertions are compiled in.
const Enabled = true

// That panics when cond is false. Safe in hot paths: msg must be a constant
// string, so nothing is boxed and nothing is allocated.
func That(cond bool, msg string) {
	if !cond {
		panic("assertion failed: " + msg)
	}
}

// Thatf is That with formatting. It allocates when called, so it belongs in
// constructors and setup code, never in a per-sample or per-block path.
func Thatf(cond bool, format string, args ...any) {
	if !cond {
		panic("assertion failed: " + fmt.Sprintf(format, args...))
	}
}
