package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

const (
	zoneCatalogCapabilityLabel = string(intent.IntentZoneCatalog)
	zoneCatalogAction          = "DescribeCompShareSupportZone"
)

// ZoneCatalogRequest has no filters: the catalog is small, and returning the
// complete live directory lets the Agent interpret partial or ambiguous user
// wording without this layer manufacturing an absence from an exact-match miss.
type ZoneCatalogRequest struct{}

func (ZoneCatalogRequest) MissingFields() []platform.MissingField { return nil }

type ZoneCatalogRecord struct {
	DisplayName string
	ZoneID      string
	Region      string
	NumericID   uint32
	AzGroup     uint32
	IsPod       bool
}

type ZoneCatalogResponse struct {
	Records []ZoneCatalogRecord
}

func zoneCatalogReadSpec() ReadCapabilitySpec[ZoneCatalogRequest, ZoneCatalogResponse] {
	return ReadCapabilitySpec[ZoneCatalogRequest, ZoneCatalogResponse]{
		Label:       zoneCatalogCapabilityLabel,
		Description: "查询平台当轮完整可用区目录，返回展示名称、ZoneID、Region 以及容器区或虚机区属性。根据完整目录判断用户提到的区域；存在多个合理候选时不要自行选择。",
		Params:      objectParam(nil),
		Handle:      zoneCatalogHandle,
		Render:      zoneCatalogRender,
	}
}

func zoneCatalogHandle(_ context.Context, _ ZoneCatalogRequest, rt ReadRuntime) (ZoneCatalogResponse, ReadResult) {
	snapshot := rt.ZoneCatalog
	if snapshot == nil || !snapshot.Available() {
		return ZoneCatalogResponse{}, ReadUnavailable("可用区目录当前不可用，请稍后重试。", nil)
	}
	records := zoneCatalogRecords(snapshot)
	if len(records) == 0 {
		return ZoneCatalogResponse{}, ReadEmpty("平台当前没有返回可用区目录。")
	}
	return ZoneCatalogResponse{Records: records}, ReadResult{}
}

func zoneCatalogRecords(snapshot *deployment.ZoneCatalogSnapshot) []ZoneCatalogRecord {
	zones := snapshot.Zones()
	out := make([]ZoneCatalogRecord, 0, len(zones))
	for _, zone := range zones {
		entry, ok := snapshot.Entry(zone)
		if !ok {
			continue
		}
		out = append(out, ZoneCatalogRecord{
			DisplayName: entry.DisplayName,
			ZoneID:      entry.Placement.Zone,
			Region:      entry.Placement.Region,
			NumericID:   entry.Placement.ZoneID,
			AzGroup:     entry.Placement.AzGroup,
			IsPod:       entry.Placement.IsPod,
		})
	}
	return out
}

func zoneCatalogRender(resp ZoneCatalogResponse) ReadResult {
	lines := make([]string, 0, len(resp.Records))
	subjects := make([]envelope.Subject, 0, len(resp.Records))
	facts := make([]envelope.Fact, 0, len(resp.Records)*3)
	for _, record := range resp.Records {
		environment := "虚机区"
		if record.IsPod {
			environment = "容器区"
		}
		name := strings.TrimSpace(record.DisplayName)
		if name == "" {
			name = record.ZoneID
		}
		lines = append(lines, fmt.Sprintf("%s：ZoneID=%s，Region=%s，环境=%s", name, record.ZoneID, record.Region, environment))
		subjects = append(subjects, envelope.Subject{ID: record.ZoneID, Name: name, Type: envelope.SubjectZone})
		facts = append(facts,
			envelope.Fact{SubjectID: record.ZoneID, Key: "region", Label: "Region", Value: record.Region, Source: envelope.FactSourceAPI},
			envelope.Fact{SubjectID: record.ZoneID, Key: "environment", Label: "环境", Value: environment, Source: envelope.FactSourceAPI},
			envelope.Fact{SubjectID: record.ZoneID, Key: "zone_numeric_id", Label: "区域编号", Value: record.NumericID, Source: envelope.FactSourceAPI},
		)
	}
	r := ReadHandled(strings.Join(lines, "\n"))
	r.ToolAction = zoneCatalogAction
	r.Envelope = &envelope.Envelope{
		Kind:          envelope.KindZoneCatalog,
		SourceActions: []string{zoneCatalogAction},
		Subjects:      subjects,
		Facts:         facts,
		Constraints: envelope.Constraints{
			DoNotInventInstances: true,
			DoNotInventMetrics:   true,
		},
	}
	return r
}
