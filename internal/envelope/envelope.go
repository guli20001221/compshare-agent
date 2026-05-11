package envelope

import "github.com/compshare-agent/internal/observability"

type Kind string

const (
	KindResourceInfo    Kind = "resource_info"
	KindMonitorQuery    Kind = "monitor_query"
	KindBillingInstance Kind = "billing_instance"
)

type SubjectType string

const (
	SubjectInstance SubjectType = "instance"
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
	DoNotAnswerAccountBill bool `json:"do_not_answer_account_bill"`
}

func Hash(env Envelope) (string, error) {
	return observability.HashTracePayload(env)
}

func AllowedIDs(env Envelope) map[string]struct{} {
	out := make(map[string]struct{}, len(env.Subjects))
	for _, subject := range env.Subjects {
		if subject.ID != "" {
			out[subject.ID] = struct{}{}
		}
	}
	return out
}

func AllowedNames(env Envelope) map[string]struct{} {
	out := make(map[string]struct{}, len(env.Subjects))
	for _, subject := range env.Subjects {
		if subject.Name != "" {
			out[subject.Name] = struct{}{}
		}
	}
	return out
}
