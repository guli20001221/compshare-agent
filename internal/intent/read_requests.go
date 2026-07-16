package intent

import "github.com/compshare-agent/internal/platform"

// The read-request contract (interface, MissingField, status vocabulary) is
// owned by internal/platform so a typed capability can implement it without
// importing this router package. These aliases keep intent.ReadRequest /
// intent.MissingField references compiling unchanged for the not-yet-migrated
// legacy request types below.
type ReadRequest = platform.ReadRequest

type MissingField = platform.MissingField

func missing(name string) MissingField { return platform.Missing(name) }

type CFSListRequest struct {
	CFS *CFSRef `json:"cfs,omitempty"`
}

type CFSRef = platform.CFSRef

func (CFSListRequest) MissingFields() []MissingField { return nil }

type CFSCreatePriceRequest struct {
	Zone         string `json:"zone"`
	TargetSizeGB int    `json:"target_size_gb"`
	ChargeType   string `json:"charge_type,omitempty"`
}

func (r CFSCreatePriceRequest) MissingFields() []MissingField {
	var out []MissingField
	if r.Zone == "" {
		out = append(out, missing("zone"))
	}
	if r.TargetSizeGB <= 0 {
		out = append(out, missing("target_size_gb"))
	}
	return out
}

type CFSUpgradePriceRequest struct {
	CFS          CFSRef `json:"cfs"`
	TargetSizeGB int    `json:"target_size_gb"`
}

func (r CFSUpgradePriceRequest) MissingFields() []MissingField {
	var out []MissingField
	if r.CFS.ID == "" {
		out = append(out, missing("cfs"))
	}
	if r.TargetSizeGB <= 0 {
		out = append(out, missing("target_size_gb"))
	}
	return out
}

type CFSRefundEstimateRequest struct {
	CFS CFSRef `json:"cfs"`
}

func (r CFSRefundEstimateRequest) MissingFields() []MissingField {
	if r.CFS.ID == "" {
		return []MissingField{missing("cfs")}
	}
	return nil
}
