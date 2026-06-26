package engine

// createPreferenceExtractionOn gates the optional LLM preference extraction for
// deploy_model image matching. Default false keeps main behavior byte-identical.
var createPreferenceExtractionOn bool

// SetCreatePreferenceExtractionEnabled toggles the deploy preference extractor.
// Boot-only (reversible by restart), matching the other engine feature gates.
func SetCreatePreferenceExtractionEnabled(v bool) { createPreferenceExtractionOn = v }

// CreatePreferenceExtractionEnabled reports whether the extractor is enabled.
func CreatePreferenceExtractionEnabled() bool { return createPreferenceExtractionOn }

// SetCreatePreferenceExtractor injects the extractor implementation for tests.
func (e *Engine) SetCreatePreferenceExtractor(extractor CreatePreferenceExtractor) {
	e.createPreferenceExtractor = extractor
}
