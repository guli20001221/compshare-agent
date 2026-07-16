package intent

// ReadRequest is implemented by one request type per platform capability.
// The model schema, decoder and handler all use the same concrete type.
type ReadRequest interface {
	MissingFields() []MissingField
}

type MissingField struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func missing(name string) MissingField { return MissingField{Name: name, Reason: "required"} }

type ResourceInfoRequest struct {
	Targets []TargetRef `json:"targets,omitempty"`
}

func (ResourceInfoRequest) MissingFields() []MissingField { return nil }

type MonitorCurrentRequest struct {
	Targets []TargetRef `json:"targets,omitempty"`
	Metrics []Metric    `json:"metrics,omitempty"`
}

func (MonitorCurrentRequest) MissingFields() []MissingField { return nil }

type MonitorHistoryRequest struct {
	Targets    []TargetRef `json:"targets,omitempty"`
	Metrics    []Metric    `json:"metrics,omitempty"`
	TimeWindow *TimeWindow `json:"time_window,omitempty"`
}

func (r MonitorHistoryRequest) MissingFields() []MissingField {
	if r.TimeWindow == nil {
		return []MissingField{missing("time_window")}
	}
	return nil
}

type GPUSpecsRequest struct {
	GPUType     string      `json:"gpu_type,omitempty"`
	DetailLevel DetailLevel `json:"detail_level,omitempty"`
}

func (GPUSpecsRequest) MissingFields() []MissingField { return nil }

type StockAvailabilityRequest struct {
	GPUType string `json:"gpu_type,omitempty"`
	Zone    string `json:"zone,omitempty"`
}

func (StockAvailabilityRequest) MissingFields() []MissingField { return nil }

type ImageListRequest struct {
	Source ImageSource `json:"source,omitempty"`
	Query  string      `json:"query,omitempty"`
	Mode   ListMode    `json:"mode,omitempty"`
}

func (ImageListRequest) MissingFields() []MissingField { return nil }

type ImageTagCatalogRequest struct{}

func (ImageTagCatalogRequest) MissingFields() []MissingField { return nil }

type ModelRepositoryRequest struct {
	Query string   `json:"query,omitempty"`
	Mode  ListMode `json:"mode,omitempty"`
}

func (ModelRepositoryRequest) MissingFields() []MissingField { return nil }

type NetworkAcceleratorStatusRequest struct {
	Targets []TargetRef `json:"targets,omitempty"`
}

func (NetworkAcceleratorStatusRequest) MissingFields() []MissingField { return nil }

type PricingRequest struct {
	GPUType  string    `json:"gpu_type"`
	GPUCount int       `json:"gpu_count,omitempty"`
	Kind     PriceKind `json:"price_kind,omitempty"`
}

func (r PricingRequest) MissingFields() []MissingField {
	if r.GPUType == "" {
		return []MissingField{missing("gpu_type")}
	}
	return nil
}

type RefundEstimateRequest struct {
	Targets []TargetRef `json:"targets"`
}

func (r RefundEstimateRequest) MissingFields() []MissingField {
	if len(r.Targets) == 0 {
		return []MissingField{missing("targets")}
	}
	return nil
}

type CFSListRequest struct {
	CFS *CFSRef `json:"cfs,omitempty"`
}

type CFSRef struct {
	ID string `json:"id"`
}

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
