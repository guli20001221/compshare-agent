package server

// Gateway field names from the console gateway body sample (2026-05-20).
// The console gateway injects these into URL params / HTTP headers /
// (future) JSON envelopes before the request reaches the agent. The agent
// trusts them as-is in TenantSourceGateway mode because the gateway has
// already authenticated (U-Csrf-Token / JWT / cookies). See
// protocol.go SECURITY note.
//
// If the backend renames any field, update this file ONLY — every other
// reference must go through these constants.
const (
	FieldTopOrganizationID = "top_organization_id"
	FieldOrganizationID    = "organization_id"
	FieldRequestUUID       = "request_uuid"
	FieldUserEmail         = "user_email"
	FieldUJWTToken         = "u_jwt_token"
	FieldProjectID         = "ProjectId"
	FieldLastRegionID      = "last_region_id"
	FieldAction            = "Action"
)

// HTTP header names the gateway uses when promoting body fields to headers.
// Lowercase per Go's canonical header key normalization done by http.Header.
const (
	HeaderTopOrganizationID = "X-Top-Org-Id"
	HeaderOrganizationID    = "X-Org-Id"
	HeaderRequestUUID       = "X-Request-Uuid"
)

// URL query parameter names — the most common gateway injection point for
// WS upgrade requests (since the WS protocol does not carry a body in the
// handshake).
const (
	QueryTopOrganizationID = "top_org_id"
	QueryOrganizationID    = "org_id"
)
