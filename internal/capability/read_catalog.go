package capability

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/intent"
	openai "github.com/sashabaranov/go-openai"
)

const ReadToolPrefix = "ReadCapability_"

const (
	readCFSList           = "cfs_list"
	readCFSCreatePrice    = "cfs_create_price"
	readCFSUpgradePrice   = "cfs_upgrade_price"
	readCFSRefundEstimate = "cfs_refund_estimate"
)

type ReadDefinition struct {
	Name        string
	Intent      intent.Intent
	Description string
	Tool        openai.Tool
	// RequestType is the concrete request struct this capability decodes into.
	// The catalog consistency test reflects it against the tool schema so a field
	// added to the struct without the schema (or vice versa) fails loudly.
	RequestType reflect.Type
	decode      func(map[string]any) (ReadRequest, error)
	// migrated is non-nil for capabilities that own a typed vertical
	// (ReadCapabilitySpec). The engine dispatches these through the typed kernel
	// instead of the legacy intent.Slots handler. nil = not yet migrated.
	migrated *RegisteredRead
}

func ReadToolName(id intent.Intent) string { return ReadToolPrefix + string(id) }

func namedReadToolName(name string) string { return ReadToolPrefix + name }

func ReadDefinitions() []ReadDefinition {
	definitions := []ReadDefinition{
		migratedReadDefinition(intent.IntentResourceInfo, NewReadCapability(resourceReadSpec())),
		migratedReadDefinition(intent.IntentMonitorQuery, NewReadCapability(monitorCurrentReadSpec())),
		migratedReadDefinition(intent.IntentMonitorHistory, NewReadCapability(monitorHistoryReadSpec())),
		migratedReadDefinition(intent.IntentGPUSpecsQuery, NewReadCapability(gpuSpecsReadSpec())),
		migratedReadDefinition(intent.IntentStockAvailability, NewReadCapability(stockReadSpec())),
		migratedReadDefinition(intent.IntentImageList, NewReadCapability(imageListReadSpec())),
		migratedReadDefinition(intent.IntentImageTagCatalog, NewReadCapability(imageTagCatalogReadSpec())),
		migratedReadDefinition(intent.IntentZoneCatalog, NewReadCapability(zoneCatalogReadSpec())),
		migratedReadDefinition(intent.IntentModelRepositoryBrowse, NewReadCapability(modelRepositoryReadSpec())),
		migratedReadDefinition(intent.IntentNetAcceleratorStatus, NewReadCapability(netAcceleratorReadSpec())),
		migratedReadDefinition(intent.IntentPricingQuery, NewReadCapability(pricingReadSpec())),
		migratedReadDefinition(intent.IntentRefundEstimate, NewReadCapability(refundReadSpec())),
		migratedReadDefinition(intent.IntentCFSInfo, NewReadCapability(cfsListReadSpec())),
		migratedReadDefinition(intent.IntentCFSInfo, NewReadCapability(cfsCreatePriceReadSpec())),
		migratedReadDefinition(intent.IntentCFSInfo, NewReadCapability(cfsUpgradePriceReadSpec())),
		migratedReadDefinition(intent.IntentCFSInfo, NewReadCapability(cfsRefundEstimateReadSpec())),
		// Unavailable capability: account-level real-time financial data is not
		// queryable; the tool is model-visible so a balance/invoice question gets a
		// deterministic non-fabricated answer + supported alternatives (P3.5).
		migratedReadDefinition(intent.Intent(accountFinanceStatusCapability), NewUnavailableCapability(accountFinanceUnavailableSpec())),
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

func newReadDefinition[T ReadRequest](name string, readIntent intent.Intent, description string, parameters map[string]any) ReadDefinition {
	toolName := namedReadToolName(name)
	return ReadDefinition{
		Name: name, Intent: readIntent, Description: description,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: toolName, Description: strings.TrimSpace(description), Parameters: parameters,
		}},
		RequestType: reflect.TypeOf(*new(T)),
		decode:      func(args map[string]any) (ReadRequest, error) { return decodeStrictRead[T](args) },
	}
}

// migratedReadDefinition wraps a typed ReadCapabilitySpec as a catalog entry.
// Its tool schema and decoder come from the spec; the engine dispatches it
// through the typed kernel (RegisteredRead.Run) rather than the legacy handler.
func migratedReadDefinition(readIntent intent.Intent, reg RegisteredRead) ReadDefinition {
	registered := reg
	return ReadDefinition{
		Name: reg.Label, Intent: readIntent, Description: reg.Description, Tool: reg.Tool,
		RequestType: reg.RequestType(),
		decode:      func(args map[string]any) (ReadRequest, error) { return registered.Decode(args) },
		migrated:    &registered,
	}
}

func DecodeReadRequest(toolName string, args map[string]any) (intent.Intent, ReadRequest, error) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == toolName {
			request, err := definition.decode(args)
			return definition.Intent, request, err
		}
	}
	return "", nil, fmt.Errorf("unknown read capability %q", toolName)
}

func ReadIntentForTool(name string) (intent.Intent, bool) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == name {
			return definition.Intent, true
		}
	}
	return "", false
}

// MigratedRead returns the typed vertical for a read tool, or false when the
// capability still runs through the legacy intent.Slots handler.
func MigratedRead(toolName string) (RegisteredRead, bool) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == toolName && definition.migrated != nil {
			return *definition.migrated, true
		}
	}
	return RegisteredRead{}, false
}

// Parameter schemas are no longer hand-written here: each capability declares a
// schemaNode field contract (field_contract.go) that is the single source for
// its tool schema, runtime validation and consistency-test expectation.
