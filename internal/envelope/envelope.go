package envelope

import "github.com/compshare-agent/internal/observability"

type Kind string

const (
	KindResourceInfo      Kind = "resource_info"
	KindDiskInfo          Kind = "disk_info"
	KindMonitorQuery      Kind = "monitor_query"
	KindGPUSpecsQuery     Kind = "gpu_specs_query"
	KindStockAvailability Kind = "stock_availability"
	KindImageList         Kind = "image_list"
	KindZoneCatalog       Kind = "zone_catalog"
	KindInstanceAccess    Kind = "instance_access"
	KindInvoiceStatus     Kind = "invoice_status"
	// KindContextualDirectReply wraps a deterministic, tool-derived plain-text
	// handler result so the answering model can combine it with understanding-
	// only conversation context without treating user text as factual evidence.
	KindContextualDirectReply Kind = "contextual_direct_reply"
)

type SubjectType string

const (
	SubjectInstance SubjectType = "instance"
	SubjectDisk     SubjectType = "disk"
	SubjectGPUModel SubjectType = "gpu_model"
	SubjectImage    SubjectType = "image"
	SubjectZone     SubjectType = "zone"
	SubjectInvoice  SubjectType = "invoice"
)

type FactSource string

const (
	FactSourceAPI      FactSource = "api"
	FactSourceComputed FactSource = "computed"
)

type Envelope struct {
	Kind          Kind        `json:"kind"`
	SourceActions []string    `json:"source_actions"`
	Subjects      []Subject   `json:"subjects"`
	Facts         []Fact      `json:"facts"`
	Computed      []Fact      `json:"computed"`
	Constraints   Constraints `json:"constraints"`
}

type Subject struct {
	ID   string      `json:"id"`
	Name string      `json:"name,omitempty"`
	Type SubjectType `json:"type"`
}

type Fact struct {
	SubjectID   string     `json:"subject_id,omitempty"`
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Value       any        `json:"value"`
	Unit        string     `json:"unit,omitempty"`
	Source      FactSource `json:"source"`
	Period      string     `json:"period,omitempty"`
	WindowStart int64      `json:"window_start,omitempty"`
	WindowEnd   int64      `json:"window_end,omitempty"`
	Aggregation string     `json:"aggregation,omitempty"`
}

type Constraints struct {
	DoNotInventInstances   bool `json:"do_not_invent_instances"`
	DoNotInventMetrics     bool `json:"do_not_invent_metrics"`
	DoNotInventZoneLabels  bool `json:"do_not_invent_zone_labels,omitempty"`
	DoNotAnswerAccountBill bool `json:"do_not_answer_account_bill"`
}

func Hash(env Envelope) (string, error) {
	return observability.HashTracePayload(env)
}
