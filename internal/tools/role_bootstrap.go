package tools

import (
	"context"
	"fmt"
	"time"
)

// RoleBootstrapper ensures the per-tenant role required for AssumeRole exists.
// STSProvider calls Bootstrap as a recovery path when AssumeRole returns
// RoleNotExist; an implementation is expected to be idempotent and safe to call
// when the role already exists.
type RoleBootstrapper interface {
	Bootstrap(ctx context.Context, companyID uint32) error
}

// Defaults for ServiceLinkedRoleBootstrapper. Values mirror the cadence used in
// uhost-extension's stop_scheduler cronjob (5 attempts with 100ms backoff, then
// 1s wait for IAM propagation), which has run safely in production.
const (
	DefaultBootstrapCreateRetries = 5
	DefaultBootstrapCreateBackoff = 100 * time.Millisecond
	DefaultBootstrapPropagateWait = 1 * time.Second
)

// ServiceLinkedRoleBootstrapper provisions a UCloud service-linked role on
// behalf of the tenant via the UAccount internal API. It mirrors the bootstrap
// flow used by uhost-extension: ICreateServiceLinkedRole, with bounded retries
// and a short propagation wait before the caller retries AssumeRole.
//
// We do NOT call IGetRole here: the RoleURN format is deterministic for
// service-linked roles (ucs:iam::<companyId>:role/<RoleName>) and matches what
// STSConfig.RoleUrnTemplate already produces, so the caller can retry AssumeRole
// with the URN it already has.
type ServiceLinkedRoleBootstrapper struct {
	Client        *UAccountClient
	ServiceName   string
	RoleName      string
	CreateRetries int
	CreateBackoff time.Duration
	PropagateWait time.Duration
}

// NewServiceLinkedRoleBootstrapper builds a bootstrapper with the documented
// defaults. serviceName/roleName must match the IAM service binding (for
// CompShare today: "compshare" / "ucs-service-role/ServiceRoleForCompshare").
func NewServiceLinkedRoleBootstrapper(client *UAccountClient, serviceName, roleName string) *ServiceLinkedRoleBootstrapper {
	return &ServiceLinkedRoleBootstrapper{
		Client:        client,
		ServiceName:   serviceName,
		RoleName:      roleName,
		CreateRetries: DefaultBootstrapCreateRetries,
		CreateBackoff: DefaultBootstrapCreateBackoff,
		PropagateWait: DefaultBootstrapPropagateWait,
	}
}

func (b *ServiceLinkedRoleBootstrapper) Bootstrap(ctx context.Context, companyID uint32) error {
	if b.Client == nil {
		return fmt.Errorf("bootstrap: UAccount client is nil")
	}
	if b.ServiceName == "" || b.RoleName == "" {
		return fmt.Errorf("bootstrap: ServiceName/RoleName not configured")
	}

	retries := b.CreateRetries
	if retries <= 0 {
		retries = 1
	}
	var lastErr error
	for i := 0; i < retries; i++ {
		err := b.Client.ICreateServiceLinkedRole(ctx, companyID, b.ServiceName)
		if err == nil {
			if b.PropagateWait > 0 {
				select {
				case <-time.After(b.PropagateWait):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		lastErr = err
		if b.CreateBackoff > 0 {
			select {
			case <-time.After(b.CreateBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("bootstrap service-linked role after %d attempts: %w", retries, lastErr)
}
