package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validMeta = `{"_meta":{"corpus_digest":"x","embed_model":"test","dim":3,"rows":2}}`

func writeSidecar(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadEmbeddingSidecarHappyPath(t *testing.T) {
	t.Parallel()
	path := writeSidecar(t,
		validMeta,
		`{"chunk_id":"c1","vector":[0.1,0.2,0.3]}`,
		`{"chunk_id":"c2","vector":[0.4,0.5,0.6]}`,
	)
	sc, err := LoadEmbeddingSidecar(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sc.Dim != 3 || sc.Rows != 2 || sc.Model != "test" {
		t.Fatalf("meta wrong: %+v", sc)
	}
	if len(sc.Vectors) != 2 {
		t.Fatalf("vectors count = %d", len(sc.Vectors))
	}
	if sc.Vectors["c1"][1] != 0.2 || sc.Vectors["c2"][2] != 0.6 {
		t.Fatalf("vector values wrong: %#v", sc.Vectors)
	}
}

func TestLoadEmbeddingSidecarFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		lines     []string
		errSubstr string
	}{
		{
			name:      "missing meta header",
			lines:     []string{`{"chunk_id":"c1","vector":[0.1,0.2,0.3]}`},
			errSubstr: "missing _meta header",
		},
		{
			name: "duplicate chunk id",
			lines: []string{
				validMeta,
				`{"chunk_id":"c1","vector":[0.1,0.2,0.3]}`,
				`{"chunk_id":"c1","vector":[0.4,0.5,0.6]}`,
			},
			errSubstr: "duplicate chunk_id",
		},
		{
			name: "dim mismatch vs meta",
			lines: []string{
				validMeta,
				`{"chunk_id":"c1","vector":[0.1,0.2]}`,
			},
			errSubstr: "vector dim 2 does not match meta dim 3",
		},
		{
			name: "empty chunk_id",
			lines: []string{
				validMeta,
				`{"chunk_id":"","vector":[0.1,0.2,0.3]}`,
			},
			errSubstr: "chunk_id is empty",
		},
		{
			name:      "empty sidecar (only meta)",
			lines:     []string{validMeta},
			errSubstr: "empty sidecar",
		},
		{
			name: "malformed row",
			lines: []string{
				validMeta,
				`{"chunk_id":"c1","vector":["not-a-number"]}`,
			},
			errSubstr: "parse row",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeSidecar(t, tc.lines...)
			_, err := LoadEmbeddingSidecar(path)
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("expected %q in error, got: %v", tc.errSubstr, err)
			}
		})
	}
}

func TestLoadPinnedCorpusWithEmbeddingsDigestMismatch(t *testing.T) {
	t.Parallel()
	// Use the real pinned corpus path from worktree.
	corpus := "../../deploy/kb/stage2b_w0.jsonl"
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("corpus not present (%v); skipping integration check", err)
	}
	tmp := t.TempDir()
	side := filepath.Join(tmp, "bad.jsonl")
	if err := os.WriteFile(side, []byte(validMeta+"\n{\"chunk_id\":\"c1\",\"vector\":[0.1,0.2,0.3]}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadPinnedCorpusWithEmbeddings(corpus, side)
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	if !strings.Contains(err.Error(), "embedding sidecar digest mismatch") {
		t.Fatalf("expected digest mismatch error, got: %v", err)
	}
}

func TestLoadPinnedCorpusWithEmbeddingsRealData(t *testing.T) {
	t.Parallel()
	corpus := "../../deploy/kb/stage2b_w0.jsonl"
	side := "../../deploy/kb/embeddings_" + CorpusDigestExpected + ".jsonl"
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("corpus not present (%v); skipping integration check", err)
	}
	if _, err := os.Stat(side); err != nil {
		t.Skipf("sidecar not present (%v); skipping integration check", err)
	}
	c, sc, err := LoadPinnedCorpusWithEmbeddings(corpus, side)
	if err != nil {
		t.Fatalf("load with pinned digests: %v", err)
	}
	if len(c.Chunks) != 182 || sc.Dim != 3072 || len(sc.Vectors) != 182 {
		t.Fatalf("unexpected loaded state: chunks=%d dim=%d vectors=%d", len(c.Chunks), sc.Dim, len(sc.Vectors))
	}
}
