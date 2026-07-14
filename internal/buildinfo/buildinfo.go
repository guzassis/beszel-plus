// Package buildinfo is the single source of product identity and build metadata.
// Version, GitCommit and BuildDate may be replaced with -ldflags at build time.
package buildinfo

import "fmt"

const (
	TechnicalAppName = "beszel"
	UpstreamVersion  = "0.18.2"
)

var (
	ProductName      = "Beszel Plus"
	ProductShortName = "Beszel Plus"
	ProductSlug      = "beszel-plus"
	RepositoryOwner  = "guzassis"
	RepositoryName   = "beszel-plus"
	RepositoryURL    = "https://github.com/guzassis/beszel-plus"
	DocumentationURL = "https://github.com/guzassis/beszel-plus#readme"
	Version          = "0.2.2-dev"
	GitCommit        = "unknown"
	BuildDate        = "unknown"
)

// VersionLine returns the standard human-readable component version.
func VersionLine(component string) string {
	return fmt.Sprintf("%s %s v%s\ncommit %s\nbuilt %s", ProductName, component, Version, GitCommit, BuildDate)
}
