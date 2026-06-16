package httpapi

import (
	"github.com/bitly/go-simplejson"
	"github.com/gin-gonic/gin"
)

// metaData is the Data payload for a successful GetMeta response.
type metaData struct {
	Model            string   `json:"Model"`
	Version          string   `json:"Version"`
	Welcome          string   `json:"Welcome"`
	SuggestedPrompts []string `json:"SuggestedPrompts"`
	MaxInputLength   int      `json:"MaxInputLength"`
	// Features advertises optional wire-protocol capabilities the backend
	// accepts this boot (e.g. "confirm_form_v1"). Omitted when none — legacy
	// responses stay byte-identical. Clients feature-detect here before
	// sending the matching Features opt-in on SendCSAgentChat.
	Features []string `json:"Features,omitempty"`
}

// handleGetMeta returns static metadata read from configuration.
func (h *Handlers) handleGetMeta(_ *gin.Context, _ BaseRequest, _ *simplejson.Json) (any, error) {
	var features []string
	if h.confirmFormEnabled {
		features = append(features, featureConfirmForm)
		if h.guidedCreateEnabled {
			features = append(features, featureGuidedCreate)
		}
	}
	return metaData{
		Model:            h.cfg.Agent.LLM.Model,
		Version:          "0.1.0",
		Welcome:          h.cfg.Agent.Meta.Welcome,
		SuggestedPrompts: h.cfg.Agent.Meta.SuggestedPrompts,
		MaxInputLength:   h.cfg.Agent.Meta.MaxInputLength,
		Features:         features,
	}, nil
}
