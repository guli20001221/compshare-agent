package httpapi

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
)

// EnginePool abstracts per-session engine lifecycle so httpapi does not depend
// directly on the agentpool package. Task 7 wires the concrete *agentpool.Pool.
type EnginePool interface {
	// Lease returns the engine for (owner, sessionID), building one on a cache miss,
	// and holds the per-entry mutex until the caller invokes the returned release func.
	// HTTP-path callers MUST use Lease to serialize concurrent Chat calls on the same session.
	Lease(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, func(), error)

	// Get returns the engine without acquiring the per-entry serialization lock.
	// Retained for backward compatibility; prefer Lease in the HTTP path.
	Get(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error)
}

// OCRRecognizer extracts text from an image. Implemented by *ocr.Client;
// the interface exists so handler tests can substitute a mock.
type OCRRecognizer interface {
	Recognize(ctx context.Context, imageDataURL string) (string, error)
}

// Handlers holds the dependencies shared by all gateway Action handlers.
type Handlers struct {
	cfg      *config.Config
	sessions store.SessionStore
	messages store.MessageStore
	feedback store.FeedbackStore
	// pool may be nil for Task 6; Task 7 wires a concrete EnginePool.
	pool          EnginePool
	traceWriter   observability.Writer
	ocrClient     OCRRecognizer
	confirmBroker *ConfirmBroker
	// confirmFormEnabled is the boot half of the editable-confirm-form double
	// gate (COMPSHARE_CONFIRM_FORM, parsed in cmd/server.go). The per-turn
	// half is the client's SendCSAgentChat Features opt-in. Both must hold
	// when a new durable turn is admitted; recovery keeps that turn's frozen
	// feature choice even if a later boot changes the rollout flag.
	confirmFormEnabled bool
	// guidedCreateEnabled is the boot half for the guided GPU create order
	// flow. It only takes effect together with confirmFormEnabled and the
	// client's guided_create_v1 feature opt-in.
	guidedCreateEnabled bool
	// turnCoordinator is the durable, globally fenced execution path. When set,
	// every WebSocket chat request (including legacy frames) uses it; the pool
	// remains only an engine factory/cache and is not a competing writer.
	turnCoordinator durableTurnCoordinator
}

// SetTurnCoordinator enables the durable turn protocol for all chat traffic.
// Passing nil is supported only by legacy unit tests and local compatibility
// harnesses; production server startup always installs a coordinator.
func (h *Handlers) SetTurnCoordinator(coordinator durableTurnCoordinator) {
	h.turnCoordinator = coordinator
}

// NewHandlers constructs a Handlers with all dependencies injected.
// pool may be nil if Chat is not yet wired.
func NewHandlers(
	cfg *config.Config,
	sessions store.SessionStore,
	messages store.MessageStore,
	feedback store.FeedbackStore,
	pool EnginePool,
	traceWriter observability.Writer,
) *Handlers {
	return &Handlers{
		cfg:           cfg,
		sessions:      sessions,
		messages:      messages,
		feedback:      feedback,
		pool:          pool,
		traceWriter:   traceWriter,
		confirmBroker: NewConfirmBroker(),
	}
}

// SetOCRClient configures the OCR client for image context injection.
// nil disables OCR; images in requests are silently ignored with a log warning.
func (h *Handlers) SetOCRClient(c OCRRecognizer) {
	h.ocrClient = c
}

// SetConfirmFormEnabled flips the boot half of the editable-confirm-form gate
// (COMPSHARE_CONFIRM_FORM). Default false keeps newly admitted turns on the
// boolean card; already-persisted turns retain their frozen feature choice.
func (h *Handlers) SetConfirmFormEnabled(enabled bool) {
	h.confirmFormEnabled = enabled
}

// SetGuidedCreateEnabled flips the boot half of the guided GPU create order
// flow. Default false keeps CreateInstanceWorkflow on the existing final-card
// confirmation flow.
func (h *Handlers) SetGuidedCreateEnabled(enabled bool) {
	h.guidedCreateEnabled = enabled
}

// buildUserContext constructs a tools.UserContext from a BaseRequest.
// Returns ErrInvalidParam if the role URN cannot be built (e.g. TopOrganizationID is zero).
func (h *Handlers) buildUserContext(base BaseRequest) (tools.UserContext, error) {
	roleUrn := ""
	if h.cfg.Agent.STS.ServiceAK != "" && h.cfg.Agent.STS.ServiceSK != "" {
		if h.cfg.Agent.STS.DefaultRoleUrn != "" {
			roleUrn = h.cfg.Agent.STS.DefaultRoleUrn
		} else {
			var err error
			roleUrn, err = tools.RoleUrnFromTemplate(h.cfg.Agent.STS.RoleUrnTemplate, base.Owner.TopOrganizationID)
			if err != nil {
				return tools.UserContext{}, ErrInvalidParam.WithMessage("failed to build role: %v", err)
			}
		}
	}
	return tools.UserContext{
		TopOrganizationID: base.Owner.TopOrganizationID,
		OrganizationID:    base.Owner.OrganizationID,
		CompanyID:         base.CompanyID,
		AccountID:         base.AccountID,
		Channel:           base.Channel,
		RoleUrn:           roleUrn,
		SessionName:       fmt.Sprintf("%d-%d", base.Owner.TopOrganizationID, base.Owner.OrganizationID),
		// ProjectId is an upstream project identifier, not an organization ID.
		// Keep it empty when the gateway did not provide one so CompShare can
		// apply its account-level default behavior.
		ProjectId:         base.ProjectID,
		Region:            h.cfg.Agent.Region,
		UserEmail:         base.UserEmail,
		ClientIP:          base.ClientIP,
	}, nil
}
