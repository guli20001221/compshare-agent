package engine

import (
	"net/url"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/knowledge"
)

// answerCitationsEnabled gates the user-visible 参考来源 block. Boot-only, frozen
// by SetAnswerCitationsEnabled, because it changes what every grounded knowledge
// answer looks like and a mid-session flip would make two turns of one
// conversation disagree about whether answers carry sources.
//
// Off means the answer ships exactly as before: markers validated, recorded on
// the citation trace, stripped for display, nothing appended.
var answerCitationsEnabled bool

// SetAnswerCitationsEnabled freezes the setting at boot.
func SetAnswerCitationsEnabled(enabled bool) { answerCitationsEnabled = enabled }

// AnswerCitationsEnabled reports the frozen setting.
func AnswerCitationsEnabled() bool { return answerCitationsEnabled }

const answerCitationsHeading = "参考来源："

// unreachableSurfacePathPrefixes lists docs paths that satisfy the surface-URL
// safety policy but are not actually served, so a citation pointing at one would
// hand the user a 404 — worse than no link, because a dead source reads as a
// fabricated one.
//
// Measured 2026-08-09 against the live docs site: of the 235 distinct
// surface_urls in the corpus, 227 return 200 and 8 return 404. All 8 are the
// OpenAPI action pages under this prefix (Create/Describe/Reboot/Reinstall/
// ResetPassword/Start/Stop/Terminate CompShareInstance); every other path is
// live. Four plausible relocations (/docs/gpus/api/, /docs/gpus/openapi/,
// lowercased, /docs/gpus/instance/) were probed and all 404, so there is no
// correct URL to substitute — the pages are simply not published.
//
// This is deliberately NOT in envelope.IsAllowedSurfaceURL: that gate decides
// what is SAFE to surface (no internal hosts, no signed URLs) and is shared with
// the trace projection, where a URL that 404s is still the right thing to
// record. Liveness only matters where a human clicks, which is here. Delete the
// entry once the docs team publishes those pages.
//
// Held as path SEGMENTS, compared element-wise: a textual prefix test would let
// a future "/docs/gpus/actionlog/" inherit this exclusion by sharing a spelling.
var unreachableSurfacePathSegments = [][]string{{"docs", "gpus", "action"}}

// citableSource is one deduped entry of the 参考来源 block.
type citableSource struct {
	title string
	url   string
}

// chunkSurfaceURL returns the chunk's public URL. SurfaceURL is the current
// field; SourceURL is the legacy one kept for corpora that have not migrated.
// Reading only the legacy field is how this silently produced nothing: the
// deployed corpus emits source_url on 0% of chunks and surface_url on 82.7%.
func chunkSurfaceURL(chunk knowledge.KBChunk) string {
	if chunk.SurfaceURL != nil {
		if trimmed := strings.TrimSpace(*chunk.SurfaceURL); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(chunk.SourceURL)
}

// pathSegments splits a URL path into its non-empty segments.
func pathSegments(path string) []string {
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

// citationURLIsReachable rejects a URL whose path is known not to be served.
func citationURLIsReachable(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	segments := pathSegments(parsed.EscapedPath())
	for _, unreachable := range unreachableSurfacePathSegments {
		if len(segments) < len(unreachable) {
			continue
		}
		matched := true
		for i, want := range unreachable {
			if segments[i] != want {
				matched = false
				break
			}
		}
		if matched {
			return false
		}
	}
	return true
}

// citableSourcesForAnswer maps the chunk ids an answer actually cited to their
// public documentation pages, in cited order, deduped by URL.
//
// It draws only from citedChunkIDs — the ids ValidateGroundedCitations RESOLVED
// against this turn's ledger — so a link can never appear for a chunk the answer
// did not use, and a fabricated marker contributes nothing. Chunks with no URL,
// an unsafe URL, or a known-dead URL are skipped silently: the block lists the
// sources that can be checked, and says nothing about the ones that cannot.
func citableSourcesForAnswer(citedChunkIDs []string, hits []knowledge.RetrievalHit) []citableSource {
	if len(citedChunkIDs) == 0 || len(hits) == 0 {
		return nil
	}
	chunkByID := make(map[string]knowledge.KBChunk, len(hits))
	for _, hit := range hits {
		if _, seen := chunkByID[hit.Chunk.ChunkID]; !seen {
			chunkByID[hit.Chunk.ChunkID] = hit.Chunk
		}
	}

	seenURL := make(map[string]struct{}, len(citedChunkIDs))
	sources := make([]citableSource, 0, len(citedChunkIDs))
	for _, chunkID := range citedChunkIDs {
		chunk, ok := chunkByID[chunkID]
		if !ok {
			continue
		}
		raw := chunkSurfaceURL(chunk)
		if !envelope.IsAllowedSurfaceURL(raw).Allowed || !citationURLIsReachable(raw) {
			continue
		}
		if _, duplicate := seenURL[raw]; duplicate {
			continue
		}
		seenURL[raw] = struct{}{}
		title := strings.TrimSpace(chunk.Title)
		if title == "" {
			title = raw
		}
		sources = append(sources, citableSource{title: title, url: raw})
	}
	if len(sources) == 0 {
		return nil
	}
	return sources
}

// renderAnswerCitations renders the 参考来源 block on its own, "" for no sources.
//
// There is no cap on the number of entries. The upstream ledger already bounds
// how many chunks can be cited at all, and only cited ones reach here (in
// practice one to three); a cap on top of that would silently drop a source the
// answer really used, which is the opposite of what the block is for.
func renderAnswerCitations(sources []citableSource) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(answerCitationsHeading)
	for _, source := range sources {
		b.WriteString("\n- [")
		b.WriteString(source.title)
		b.WriteString("](")
		b.WriteString(source.url)
		b.WriteString(")")
	}
	return b.String()
}

// withAnswerCitations composes the block onto the text the user reads. The
// caller keeps the UNcomposed text for history — see answerCitationsBlockThisTurn.
func withAnswerCitations(display, block string) string {
	if block == "" || strings.TrimSpace(display) == "" {
		return display
	}
	return strings.TrimRight(display, "\n") + "\n\n" + block
}

// recordAnswerCitations stores this turn's block for the final-text branch to
// compose. It deliberately does NOT modify the answer: the reply recorded in
// e.messages must stay free of rendered URLs, so display and history part company
// at exactly one place rather than being separated later by string surgery.
//
// A no-op unless the flag is on and the cited evidence resolves to pages that can
// actually be opened.
func (e *Engine) recordAnswerCitations(citedChunkIDs []string) {
	if !answerCitationsEnabled {
		return
	}
	e.answerCitationsBlockThisTurn = renderAnswerCitations(citableSourcesForAnswer(citedChunkIDs, e.searchKnowledgeHitsThisTurn))
}
