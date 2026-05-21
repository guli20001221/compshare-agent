package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/compshare-agent/internal/governance"
)

// TenantSource selects the trust chain for caller identity. Chosen at
// process startup via COMPSHARE_TENANT_SOURCE env (default "gateway").
type TenantSource string

const (
	// TenantSourceGateway: agent sits behind the console gateway. The
	// gateway authenticates and injects top_organization_id /
	// organization_id into URL params or HTTP headers. Agent trusts the
	// gateway-provided values without further verification.
	//
	// This is the 2026-05-21 default — confirmed with the user. If the
	// deployment topology changes (agent exposed directly to clients),
	// flip COMPSHARE_TENANT_SOURCE=jwt and the agent will verify
	// u_jwt_token signatures instead.
	TenantSourceGateway TenantSource = "gateway"

	// TenantSourceJWT: not implemented in PR4 / A3. PR5 / A6 may flesh
	// this out when the backend confirms whether the agent will be
	// directly exposed.
	TenantSourceJWT TenantSource = "jwt"
)

// TenantCtx is the per-request identity established by the WS handshake.
// SECURITY: the agent NEVER reads tenant fields from the WS payload
// (ClientMessage). All construction happens in parseTenant before the
// reader loop starts.
type TenantCtx struct {
	TopOrgID     int64
	OrgID        int64
	UJWTToken    string
	LastRegionID int64
	ProjectID    string
	UserEmail    string
}

// Subject returns the rate-limit subject derived from the tenant identity.
// Centralizes the SubjectKeyFromTenant call so handlers can't accidentally
// hash other fields instead.
func (t TenantCtx) Subject() string {
	return governance.SubjectKeyFromTenant(t.TopOrgID, t.OrgID)
}

// parseTenant resolves the caller's identity from request metadata. The
// dispatch on s.tenantSource keeps each trust chain isolated; future modes
// (JWT) add a case without touching the others.
//
// Returns an error when no acceptable identity can be established — callers
// MUST translate that error into a WS close + ErrCodeUnauthorized frame.
// Returning a zero TenantCtx with no error is forbidden: a zero ctx
// would hash to the AnonymousSubjectKey bucket and let unauthenticated
// callers share one quota.
func (s *Server) parseTenant(r *http.Request) (TenantCtx, error) {
	switch s.tenantSource {
	case TenantSourceJWT:
		return TenantCtx{}, errors.New(
			"COMPSHARE_TENANT_SOURCE=jwt is not implemented yet; " +
				"set COMPSHARE_TENANT_SOURCE=gateway or implement JWT verification first")
	case TenantSourceGateway, "":
		return s.parseTenantFromGateway(r)
	default:
		return TenantCtx{}, fmt.Errorf("unknown tenant source: %q", s.tenantSource)
	}
}

// parseTenantFromGateway reads URL params first, falling back to headers.
// URL params win because they're the most common gateway injection point
// for WS upgrades (headers are also used when the gateway forwards the
// upgrade through a header-mutating proxy chain).
//
// Both top_organization_id and organization_id are needed; either alone is
// insufficient because rate-limit subjects mix both into the hash and
// downstream queries filter on both.
func (s *Server) parseTenantFromGateway(r *http.Request) (TenantCtx, error) {
	var t TenantCtx
	if v := r.URL.Query().Get(QueryTopOrganizationID); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.TopOrgID = n
		} else {
			return t, fmt.Errorf("invalid %s: %w", QueryTopOrganizationID, err)
		}
	}
	if v := r.URL.Query().Get(QueryOrganizationID); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.OrgID = n
		} else {
			return t, fmt.Errorf("invalid %s: %w", QueryOrganizationID, err)
		}
	}
	if t.TopOrgID == 0 {
		if v := r.Header.Get(HeaderTopOrganizationID); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				t.TopOrgID = n
			} else {
				return t, fmt.Errorf("invalid %s header: %w", HeaderTopOrganizationID, err)
			}
		}
	}
	if t.OrgID == 0 {
		if v := r.Header.Get(HeaderOrganizationID); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				t.OrgID = n
			} else {
				return t, fmt.Errorf("invalid %s header: %w", HeaderOrganizationID, err)
			}
		}
	}
	if t.TopOrgID == 0 || t.OrgID == 0 {
		return t, errors.New("missing tenant identifiers (top_organization_id + organization_id)")
	}
	return t, nil
}
