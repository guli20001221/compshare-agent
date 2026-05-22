package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// UserContext carries the per-request identity for multi-tenant STS flows.
type UserContext struct {
	TopOrganizationID uint32
	OrganizationID    uint32
	RoleUrn           string
	SessionName       string
	ProjectId         string
	Region            string
}

type userKey struct{}

// WithUser stores a UserContext in ctx.
func WithUser(ctx context.Context, u UserContext) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom retrieves a UserContext previously stored by WithUser.
func UserFrom(ctx context.Context) (UserContext, bool) {
	u, ok := ctx.Value(userKey{}).(UserContext)
	return u, ok
}

// RoleUrnFromTemplate formats template with topOrg (as %d) to produce a RoleUrn.
// Returns an error when topOrg is zero or template is empty.
func RoleUrnFromTemplate(template string, topOrg uint32) (string, error) {
	if topOrg == 0 {
		return "", fmt.Errorf("top_organization_id must be > 0")
	}
	if template == "" {
		return "", fmt.Errorf("role_urn_template is empty")
	}
	return fmt.Sprintf(template, topOrg), nil
}

// SubjectKeyFromUser produces a stable, opaque hash key for a UserContext pair.
// Returns ("anonymous", false) when either ID is zero.
func SubjectKeyFromUser(u UserContext) (string, bool) {
	if u.TopOrganizationID == 0 || u.OrganizationID == 0 {
		return "anonymous", false
	}
	raw := fmt.Sprintf("%d:%d", u.TopOrganizationID, u.OrganizationID)
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:]), true
}
