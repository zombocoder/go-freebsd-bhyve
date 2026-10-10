module github.com/zombocoder/go-freebsd-bhyve

go 1.22

require github.com/zombocoder/go-freebsd-vmm v0.0.0

// go-freebsd-vmm is developed alongside this module and is not published
// yet. Drop this once it has a tag.
replace github.com/zombocoder/go-freebsd-vmm => ../go-freebsd-vmm
