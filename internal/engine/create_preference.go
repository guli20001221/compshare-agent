package engine

// createPreferenceExtractionOn gates the optional LLM preference extraction for
// create_instance and deploy_model preference matching. Default on; set
// COMPSHARE_CREATE_PREF_EXTRACTOR=0/off/false to disable it at boot.
var createPreferenceExtractionOn = true

// SetCreatePreferenceExtractionEnabled toggles the create/deploy preference extractor.
// Boot-only (reversible by restart), matching the other engine feature gates.
func SetCreatePreferenceExtractionEnabled(v bool) { createPreferenceExtractionOn = v }

// CreatePreferenceExtractionEnabled reports whether the extractor is enabled.
func CreatePreferenceExtractionEnabled() bool { return createPreferenceExtractionOn }

// SetCreatePreferenceExtractor injects the extractor implementation for tests.
func (e *Engine) SetCreatePreferenceExtractor(extractor CreatePreferenceExtractor) {
	e.createPreferenceExtractor = extractor
}
