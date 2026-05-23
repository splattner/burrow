package version

// Version is the current binary version, injected at build time via ldflags:
//
//	-X github.com/splattner/burrow/internal/version.Version=<version>
var Version = "dev"
