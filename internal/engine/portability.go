package engine

import (
	"fmt"
	"net/http"
	"strings"
)

// Portability checks reject input that OpenSearch accepts but that does not
// carry over to every deployment target, in particular Amazon OpenSearch
// Serverless: a test passing on osmem must pass on a managed domain and on
// Serverless alike.

// CollectionType is the Amazon OpenSearch Serverless collection type osmem
// emulates. The zero value emulates an OpenSearch cluster (managed domain).
type CollectionType string

// Serverless collection types.
const (
	CollectionSearch       CollectionType = "search"
	CollectionTimeSeries   CollectionType = "timeseries"
	CollectionVectorSearch CollectionType = "vectorsearch"
)

// ParseCollectionType parses a Serverless collection type name.
func ParseCollectionType(s string) (CollectionType, error) {
	switch t := CollectionType(strings.ToLower(s)); t {
	case CollectionSearch, CollectionTimeSeries, CollectionVectorSearch:
		return t, nil
	}
	return "", fmt.Errorf("unknown serverless collection type %q (want search, timeseries or vectorsearch)", s)
}

// portableNameChars are the characters a Serverless resource name may hold
// after its first character (the ResourceName pattern of the Serverless
// API: [a-z;0-9&$%][+.~=\-_a-z;0-9&$%]*).
func portableNameChar(r rune, first bool) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case strings.ContainsRune(";&$%", r):
		return true
	}
	return !first && strings.ContainsRune("+.~=-_", r)
}

// nonPortableChars returns why name is outside the Serverless resource name
// pattern, or "". A leading '.' is judged by the dot rule, not here.
func nonPortableChars(name string) string {
	for i, r := range name {
		first := i == 0
		if first && r == '.' {
			continue
		}
		if !portableNameChar(r, first) {
			return "must contain only [a-z0-9+.~=_;&$%-] and must start with [a-z0-9;&$%]"
		}
	}
	return ""
}

// checkPortableIndexName applies the portability rules after the core index
// name checks: a leading '.' is rejected unless AllowDotNames is set, and a
// name outside the Serverless character set is rejected when emulating
// Serverless and reported as a warning otherwise.
func (c *Cluster) checkPortableIndexName(name string) error {
	if strings.HasPrefix(name, ".") && !c.AllowDotNames {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], must not start with '.'", Index: name}
	}
	if why := nonPortableChars(name); why != "" {
		if c.Serverless != "" {
			return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], " + why, Index: name}
		}
		c.warn("index name [%s] is not portable to OpenSearch Serverless: it %s", name, why)
	}
	return nil
}

// checkPortableAliasName is checkPortableIndexName for aliases, which also
// must be lowercase (OpenSearch itself accepts upper case alias names).
func (c *Cluster) checkPortableAliasName(name string) error {
	if strings.ToLower(name) != name {
		return errInvalidAliasName(name, "must be lowercase")
	}
	if strings.HasPrefix(name, ".") && !c.AllowDotNames {
		return errInvalidAliasName(name, "must not start with '.'")
	}
	if why := nonPortableChars(name); why != "" {
		if c.Serverless != "" {
			return errInvalidAliasName(name, why)
		}
		c.warn("alias name [%s] is not portable to OpenSearch Serverless: it %s", name, why)
	}
	return nil
}

// rejectsDocumentIDs reports whether the emulated collection type refuses
// client supplied document IDs (and with them _create/{id}, _update/{id}
// and upserts): only search collections accept them.
func (c *Cluster) rejectsDocumentIDs() bool {
	return c.Serverless == CollectionTimeSeries || c.Serverless == CollectionVectorSearch
}

// errDocumentIDUnsupported is the Serverless answer to a write naming a
// document ID in a time series or vector search collection.
func errDocumentIDUnsupported(op string) *Error {
	return errIllegalArgument("Document ID is not supported in %s operation request", op)
}

// serverlessFixedSettings are index settings Serverless manages itself.
var serverlessFixedSettings = []string{"number_of_shards", "number_of_replicas", "refresh_interval"}

// warnServerlessSettings warns about settings a Serverless collection does
// not let clients choose. settings is a raw settings object.
func (c *Cluster) warnServerlessSettings(index string, settings M) {
	if c.Serverless == "" || len(settings) == 0 {
		return
	}
	idx := normalizeSettings(settings)["index"].(M)
	for _, k := range serverlessFixedSettings {
		if _, ok := idx[k]; ok {
			c.warn("index [%s]: setting [index.%s] is managed by OpenSearch Serverless and cannot be set there", index, k)
		}
	}
}
