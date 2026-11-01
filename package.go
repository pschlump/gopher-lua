// gopher-lua (pschlump fork): a Lua 5.5.1 virtual machine and compiler in
// pure Go — the embedded scripting engine for the Ultima project. Carved
// from glasm @ 0affa95 (2026-11-01); both are forks of yuin/gopher-lua
// (upstream copyright and license in LICENSE).
package lua

const PackageName = "GopherLua"
const PackageAuthors = "Yusuke Inuzuka"

// PackageVersion is overridden at link time by the Makefile (make build) via
// -ldflags "-X github.com/pschlump/gopher-lua.PackageVersion=<version>"; the
// literal below is the fallback for plain go builds and tests.
var PackageVersion = "5.5.1-carve.0"

// PackageCopyRight must stay a var: it is computed from PackageVersion at
// package init, after the linker has applied any -X override.
var PackageCopyRight = PackageName + " " + PackageVersion + " (Lua 5.5.1 in Go) Copyright (C) 2015 -2017 " + PackageAuthors + " Copyright (C) 2026 Philip Schlump"
