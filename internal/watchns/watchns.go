// Package watchns turns a --watch-namespaces flag into controller-runtime cache
// options. Namespace-scoped RBAC (a Role per watched namespace) only works if the
// informer cache also lists/watches per namespace; the default cache issues
// cluster-wide LIST/WATCH that a Role cannot authorize and the binary crash-loops.
package watchns

import (
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// Parse splits a comma-separated namespace list, trimming whitespace and dropping
// empties and duplicates. An empty result means "all namespaces".
func Parse(csv string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, ns := range strings.Split(csv, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// CacheOptions restricts the cache to the given namespaces. With none given it
// returns zero options, which is controller-runtime's cluster-wide default.
func CacheOptions(namespaces []string) cache.Options {
	if len(namespaces) == 0 {
		return cache.Options{}
	}
	byNS := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		byNS[ns] = cache.Config{}
	}
	return cache.Options{DefaultNamespaces: byNS}
}
