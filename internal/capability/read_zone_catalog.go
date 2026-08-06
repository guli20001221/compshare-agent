package capability

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

const (
	zoneCatalogCapabilityLabel = string(intent.IntentZoneCatalog)
	zoneCatalogAction          = "DescribeCompShareSupportZone"
)

// ZoneCatalogRequest optionally narrows the live catalog to one literal display
// name or ZoneID. Empty query lists the complete catalog. The capability does no
// semantic guessing: the Agent understands the question, while this handler only
// verifies an exact catalog relation.
type ZoneCatalogRequest struct {
	Query string `json:"query,omitempty"`
}

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
		Description: "一次查询平台当轮完整可用区目录，返回展示名称、ZoneID、Region 以及容器区或虚机区属性。query 仅用于核实用户本轮明确提到的单个名称或 ZoneID；列出全部区域时留空。",
		Params:      objectParam(map[string]schemaNode{"query": stringParam()}),
		Handle:      zoneCatalogHandle,
		Render:      zoneCatalogRender,
	}
}

func zoneCatalogHandle(_ context.Context, req ZoneCatalogRequest, rt ReadRuntime) (ZoneCatalogResponse, ReadResult) {
	snapshot := rt.ZoneCatalog
	if snapshot == nil || !snapshot.Available() {
		return ZoneCatalogResponse{}, ReadUnavailable("可用区目录当前不可用，请稍后重试。", nil)
	}
	records := zoneCatalogRecords(snapshot)
	if len(records) == 0 {
		return ZoneCatalogResponse{}, ReadEmpty("平台当前没有返回可用区目录。")
	}
	query := normalizeZoneLookup(req.Query)
	if query == "" {
		return ZoneCatalogResponse{Records: records}, ReadResult{}
	}
	matches := make([]ZoneCatalogRecord, 0, 1)
	for _, record := range records {
		if normalizeZoneLookup(record.ZoneID) == query || normalizeZoneLookup(record.DisplayName) == query {
			matches = append(matches, record)
		}
	}
	switch len(matches) {
	case 0:
		return ZoneCatalogResponse{}, ReadEmpty(fmt.Sprintf("当前可用区目录中没有找到 %s。", strings.TrimSpace(req.Query)))
	case 1:
		return ZoneCatalogResponse{Records: matches}, ReadResult{}
	default:
		labels := make([]string, 0, len(matches))
		for _, match := range matches {
			labels = append(labels, match.DisplayName+"（"+match.ZoneID+"）")
		}
		return ZoneCatalogResponse{}, ReadConflict("该区域名称对应多个可用区，请明确选择：" + strings.Join(labels, "、"))
	}
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

func normalizeZoneLookup(value string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value)))
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
