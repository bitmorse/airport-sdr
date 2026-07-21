//go:build !assert

package assert

// Enabled reports whether assertions are compiled in.
const Enabled = false

// That is a no-op in normal builds and is inlined away by the compiler.
func That(cond bool, msg string) {}

// Thatf is a no-op in normal builds. It is still confined to cold paths, since
// boxing its arguments is not guaranteed to be optimised out.
func Thatf(cond bool, format string, args ...any) {}
