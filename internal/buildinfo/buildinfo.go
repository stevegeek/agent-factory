// Package buildinfo is the build's identity. build.sh sets both values with
// -ldflags -X; a plain `go build` leaves them "dev". Every version shown
// anywhere (factory version, MCP serverInfo.version, Foreman's footer) comes
// from here, never from debug.BuildInfo, because builds use -buildvcs=false.
package buildinfo

// Version is the commit the binaries were built from. Set only by the
// linker (-X); no code assigns it.
var Version = "dev"

// GoVersion is the toolchain that built the binaries. Set only by the linker.
var GoVersion = "dev"

// String is the `factory version` line: "<commit> <go version>".
func String() string {
	return Version + " " + GoVersion
}
