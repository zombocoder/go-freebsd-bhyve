//go:build !freebsd

// Package bhyve has no implementation for this platform.
//
// It runs and supervises FreeBSD virtual machines by driving bhyve(8)
// and reaching into the result through go-freebsd-vmm, so there is
// nothing it could usefully do elsewhere. Rather than offer an API that
// compiles and then fails at run time, the build stops here with the
// reason.
package bhyve

// The line below is a deliberate type error. It is the only way to make
// the compiler print an explanation rather than a list of undefined
// symbols from whatever imported this package.
//
// Expect:
//
//	invalid operation: "go-freebsd-bhyve requires FreeBSD" - 0
//	(mismatched types untyped string and untyped int)
var _ = "go-freebsd-bhyve requires FreeBSD" - 0
