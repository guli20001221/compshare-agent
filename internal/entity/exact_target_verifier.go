package entity

import (
	"strings"
	"time"
)

// ExistenceVerdict is the three-state outcome of verifying that an exact instance
// id currently exists in an account. It is deliberately NOT a bool: a query that
// cannot complete (transport error, timeout, unusable response) is Unavailable —
// a dependency failure the caller must surface as its own outage, never a false
// "the instance does not exist". Collapsing Unavailable into NotFound would blame
// the user's target for the server's failed query.
type ExistenceVerdict int

const (
	// ExistenceUnavailable: existence could not be determined — the point-query
	// failed, timed out, or returned an unusable response. NOT a NotFound.
	ExistenceUnavailable ExistenceVerdict = iota
	// ExistenceVerified: the id is confirmed present in the account.
	ExistenceVerified
	// ExistenceNotFound: the account authoritatively does not contain the id.
	ExistenceNotFound
)

// DescribeInstanceFunc performs a point DescribeCompShareInstance(id) under the
// account's credentials. ok reports whether the CALL itself succeeded: ok=false
// means the query could not complete (a dependency failure), which is distinct
// from a successful response that simply did not include the id.
type DescribeInstanceFunc func(id string) (raw map[string]any, ok bool)

// VerifyExactTarget decides whether an exact instance id currently exists in the
// account backing snap, as of `at`. It is the single existence primitive the read
// and write paths share, so neither re-implements the freshness / echo rules:
//
//	fresh+complete registry hit          -> Verified (no network)
//	fresh+complete registry, id absent   -> NotFound (authoritative, no network)
//	otherwise (cold/stale/truncated/     -> point-query; the RESPONSE must echo the
//	  invalidated registry)                 same id (Verified) else NotFound; a
//	                                         failed query is Unavailable.
//
// A stale or invalidated registry never answers from its snapshot: a released
// instance lingering in a one-hour-old cache must not read as Verified, and a
// newly-created one missing from it must not read as NotFound.
func VerifyExactTarget(snap RegistrySnapshot, id string, at time.Time, describe DescribeInstanceFunc) ExistenceVerdict {
	if strings.TrimSpace(id) == "" {
		return ExistenceNotFound
	}
	if snap.FreshAndCompleteAt(at) {
		if _, ok := snap.Instances[id]; ok {
			return ExistenceVerified
		}
		// Fresh, complete, and the id is not present: a real, authoritative absence.
		return ExistenceNotFound
	}
	if describe == nil {
		return ExistenceUnavailable
	}
	raw, ok := describe(id)
	if !ok {
		return ExistenceUnavailable
	}
	if DescribeResponseHasID(raw, id) {
		return ExistenceVerified
	}
	return ExistenceNotFound
}

// DescribeResponseHasID reports whether a DescribeCompShareInstance response
// really carries the exact id. Existence is proven only by the RESPONSE echoing
// the same UHostId — never by the request having asked for it (the API can ignore
// an unknown filter and return an empty or unrelated set). Shared by the write
// point-query and the read (resource_info) path so both apply one rule.
func DescribeResponseHasID(raw map[string]any, id string) bool {
	if raw == nil || strings.TrimSpace(id) == "" {
		return false
	}
	set, _ := raw["UHostSet"].([]any)
	for _, row := range set {
		m, _ := row.(map[string]any)
		if m == nil {
			continue
		}
		if got, _ := m["UHostId"].(string); got == id {
			return true
		}
	}
	return false
}
