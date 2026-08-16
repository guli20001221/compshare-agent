# RAG V2 preprocessing and release pipeline

V2 rebuilds the platform corpus from pinned public source documents and extends the external corpus from source snapshots. It does not redact or rewrite public text. Images are build-time evidence: VL converts them into structured captions, visible text, controls, and spatial relations; runtime chunks contain no hosted image or clickable image URL. Private chats, tickets, internal admin pages, temporary signed links, and unpinned inputs are rejected at source selection instead of being redacted later.

The preprocessing pipeline is reusable and independent of the runtime Agentic
RAG policy. It emits a runtime-compatible JSONL projection plus traceability
fields that older loaders may ignore. Runtime query planning, `SearchKnowledge`,
retrieval modes, embedding, reranking, citation validation, and answer synthesis
can evolve separately without changing the source-lock, build, validation, and
promotion stages described here.

## Inputs

- `compshare-docs/main` checked out at an explicit commit
- three public FAQ ZIP exports, in this order: model package, ComfyUI base image, platform usage
- three external ZIP snapshots, in this order: ComfyUI mirror, digital-human, voice/audio
- the existing external corpus, retained as an explicitly non-rebuildable legacy slice because its original source snapshot is unavailable

## Build

```powershell
python -m scripts.rag_v2.build `
  --internal-docs F:\compshare-agent-rag-v2-sources\compshare-docs `
  --internal-revision 8a81268e3d275d4767d045554680e7c5ddf82a9d `
  --faq-zip 'deploy\kb\v2\sources\优云智算模型套餐FAQ.zip' `
  --faq-zip 'deploy\kb\v2\sources\ComfyUI基础镜像常见问题解答（持续更新中）.zip' `
  --faq-zip 'deploy\kb\v2\sources\优云智算使用问题FAQ.zip' `
  --external-zip 'deploy\kb\v2\sources\comfyui-mirror-docs.zip' `
  --external-zip 'deploy\kb\v2\sources\digital-human-docs.zip' `
  --external-zip 'deploy\kb\v2\sources\voice-audio-docs.zip' `
  --legacy-external deploy\kb\v2\legacy_external_lock.jsonl `
  --out-dir deploy\kb\v2 `
  --env F:\compshare-agent\.env.local `
  --valid-from 2026-08-16 `
  --external-valid-from 2026-08-14
```

`--external-valid-from` defaults to `--valid-from`, so a single date behaves
exactly as before. Pass the PREVIOUS external date whenever the external
snapshots did not change — which is every docs-only rebuild. One date for both
corpora restamps all 1189 external chunks, moves the corpus digest, and rewrites
the 60 MB sidecar under a new digest-bearing filename with two Go pins following
it, while re-embedding nothing (`chunk_repr` is title/patterns/content and knows
no dates). Measured as git stores them, holding it back costs ~7.4 MB of history
per release instead of ~23.8 MB. Read the current value off the released corpus
rather than remembering it:

```powershell
python -m scripts.rag_v2.release_inputs corpus-valid-from deploy\kb\external_w0.jsonl
```

`--skip-vl` and `--skip-semantic` are only for deterministic unit tests and local diagnosis. A release build must process all referenced images and must fail closed when a required asset cannot be extracted.

External snapshots must include every source-local image that carries workflow,
input/output, architecture, or UI evidence. The pipeline does not fetch an
unpinned current copy from an upstream repository when a pinned ZIP omitted the
file. Remote images are retried, GitHub `blob` links are normalized to raw
content, animated GIF/WebP inputs are flattened into a 4-frame contact sheet,
and decorative badges are excluded before VL. Missing external images are
recorded in `asset_report.json` and omitted from runtime text; no caption or
placeholder is fabricated.

Flattening is **not optional**: the contact sheet is what the model is shown, so
it decides caption content, and captions are cached across builds under a
contract digest. Two boundaries, deliberately different widths:

- **Pillow is required for any GIF/WebP**, because telling a static one from an
  animated one already needs a decoder.
- **The pinned Pillow version is required only when a sheet is actually
  rendered**, i.e. for a genuinely animated image. A single-frame GIF/WebP is
  handed to the model untouched, so no version can change what it sees, and
  failing that build would be failing over a rendering that never happens.

The pinned version is part of the caption contract, so changing it re-earns
every animated caption rather than silently mixing two renderings under one
digest. It also, today, re-earns every OTHER caption too — the contract is
global — which is why raising the pin is a deliberate act and not a routine
dependency bump.

```bash
python -m pip install -r scripts/rag_v2/requirements.txt
```

`pypdf` and `pymupdf` are in there for the same reason: the usage FAQ ships two
PDFs and the extractor hard-raises without either. PyMuPDF renders the PDF page
images the VL model is shown, so it decides caption content the way Pillow does —
but it is **not** in `caption_contract_digest()`, so bumping it re-earns those
captions with nothing saying so. `requirements.txt` states why that is deliberate
and what the real fix is.

### The six ZIP inputs are vendored

`deploy/kb/v2/sources/` holds all six, byte-identical to what built the shipped
corpus. Only `--internal-docs` still comes from outside the repo, and that is a
git remote a machine can clone — which is what makes an unattended rebuild
possible at all.

`VendoredSourceTests` joins each file against the `sha256` its
`release_manifest.json` entry already records, and fails on a stray file in the
directory. Present is a weaker claim than *is the one that built the corpus*, and
the ZIPs are passed **positionally** (`FAQ_IDS` / `EXTERNAL_PACKAGES` map by
order, not by name), so a re-export dropped in beside the originals is exactly how
the wrong input gets built silently. When a FAQ is genuinely re-exported, replace
the file and re-run the build — the test then fails until the manifest is
regenerated, which is the intended sequence, not an obstacle.

When a remote image cannot be revalidated and cached bytes are on hand, the
build keeps the cached bytes rather than failing over a transient error — but
it records the degrade in `asset_report.json` under `degradations`, and for
platform (non-third-party) sources that is an error the release gate blocks on
unless `--allow-stale-remote` is passed deliberately.

The output is an immutable release bundle containing both corpora, copied local assets, model caches, source locks, and a release manifest. Model caches are build artifacts and should not be committed.

The committed release keeps the promoted runtime sidecars only. Candidate
sidecars in `deploy/kb/v2` are transient because they duplicate roughly 150 MB
of runtime data.

## Cutting a release

One command covers build → refresh embeddings → diff → promote → repin →
offline bundle check:

First read the release production is serving, because the candidate has to
declare what it is built on:

```powershell
kubectl -n prj-ucompshare-prod exec compshare-kb-0 -- wget -qO- http://127.0.0.1:8088/healthz
```

```powershell
python -m scripts.rag_v2.release --env .env.local `
  --parent-release-id kb-release-<that release_id> `
  --build-arg --internal-docs --build-arg <docs-checkout> `
  --build-arg --internal-revision --build-arg <sha> `
  --build-arg --valid-from --build-arg 2026-08-16 `
  ... (one --build-arg per token; see scripts/rag_v2/build.py for the full list)
```

That id is recorded in `deploy/kb/v2/release_base.json` and committed with the
corpus. The import job refuses a candidate whose recorded parent is not what
production is currently serving, and passes it to the worker as
`--parent-release-id` so the later publish is a compare-and-swap. It cannot be
derived at import time: reading the active release then records whatever is live
when the job runs, and the two differ exactly when it matters — build on R0, let
R1 publish while the candidate is in review, and an import-time read would file
the candidate as R1's child, so publishing it overwrites content it never saw
while the base match reports success.

It does **not** publish. Publication is two separate manual GitLab jobs so that
a person reads the diff in between:

1. `import-knowledge-release` — copies the promoted corpora into a throwaway
   Pod, imports and validates a **candidate**, and leaves the active pointer
   alone. Nothing a user sees changes. It attaches `release_diff.md` and
   `asset_report.json` as job artifacts.

   It refuses to start unless **both** `release_base.json` and `release_diff.md`
   are committed under `deploy/kb/v2`, before it touches the cluster. Neither is
   produced by anything but `release.py`, so a corpus committed without it cannot
   be imported at all — that is deliberate, since such a candidate declares no
   parent and offers nothing to review. It also means the first import after this
   lands must come from an orchestrated release, not from an edited corpus.
2. `publish-knowledge-release` — runs `--action publish` on the serving pod,
   waits for `/healthz` to report the new `kb_version`, and then runs a live
   retrieval smoke, because moving the pointer and the release being usable are
   different claims. It retries — the index reloads on its own interval — and on
   persistent failure it prints the exact rollback command with the parent
   release id filled in. It does **not** roll back by itself: a failing smoke can
   also mean the index has not finished reloading, a person is present when
   publish is clicked, and automatic recovery belongs with automatic publication
   rather than before it.

`release_diff.md` is written into `deploy/kb/v2` and is meant to be committed
with the corpus, because the reviewer needs it before the candidate exists. It
compares chunks by SLOT rather than by `chunk_id` (which digests the content, so
every edit would otherwise read as one deletion plus one addition), and it pairs
renamed documents — the `pages/` → `content/` migration renamed 227 documents at
once, which rendered as 232 additions beside 235 deletions before pairing and as
5 additions and 9 deletions after.

Incrementality lives entirely in the build. Captions, semantic plans and
embeddings are all content-addressed and skip unchanged work; the database
always receives a complete immutable candidate and an atomic pointer swap. Do
not make the ingest incremental — the atomic swap is what makes a release
reviewable and reversible.

Rolling back is `rollback-knowledge-release` (or
`compshare-kb-worker --action rollback --release-id <old-id>`). The data was
never the problem — retired releases keep their chunks and vectors — the missing
piece was the write that moves the pointer back. Note that the recovery this
file used to document, flipping the old row to `validated` and re-publishing,
was **incomplete**: every CI import persists `require_base_match`, so publish
also compares the active pointer against that row's recorded parent, and after a
bad publish the pointer has moved, so the old release no longer matches its own
parent and re-publishing returns `ErrStaleCandidate`.

Both the rollback action and the smoke live in `compshare-kb`, so the kb merge
request has to be merged and deployed before either job can work — the same
ordering `--parent-release-id` already requires.

## Unattended releases

A scheduled GitLab pipeline asks one question — has `compshare-docs` moved since
the corpus was built — and on almost every tick the answer is no and nothing
happens. `knowledge-tick` is deliberately a separate, cheap job on the kubectl
image: it compares `release_manifest.json`'s recorded revision against
`git ls-remote`, and only when they differ does it read the active release id out
of `/healthz` and hand both to `build-knowledge-candidate`.

The build job does exactly what the manual command above does, then runs the
gate, commits, force-pushes one fixed branch and opens at most one merge request.
It does **not** merge, import or publish. Auto-publish is a later phase and is
gated on the gate having been observed both agreeing with a human and
demonstrably red on a broken candidate.

Cadence is bounded by git rather than by build cost: every release rewrites the
embedding sidecars in full (60.2 MB external + 26.6 MB internal raw; 15.3 + 6.8
MB compressed the way git stores them). Weekly is about 30 MB a month. A tick per
docs commit would be up to ~290 MB a month even with the external corpus held
frozen, so raising the cadence means moving the sidecars out of git first.

Three CI variables carry what the job cannot derive: read access to
compshare-docs, a token that can push and open a merge request here, and a
ModelVerse key authorized for **all three** of `Qwen/Qwen3-VL-235B-A22B-Instruct`,
`qwen3.7-max` and `qwen3-embedding-8b` — the retrieval-side key, not the terra
answer key, which is scoped to terra alone.

## The release gate

`release_gate.py` decides whether every difference in a candidate is
*attributable*. That is a weaker claim than "the content is correct" and a much
stronger one than anything a diff can offer: the compshare-docs half of the
corpus inherits the review its upstream merge request already had, and the half
with no upstream reviewer — the three after-sales FAQ ZIPs and the scraped
external snapshots — is not judged at all, only proven not to have moved.

```powershell
python -m scripts.rag_v2.release_gate `
  --release-dir deploy\kb\v2 `
  --released-internal <pre-build copy of deploy\kb\stage2b_w0.jsonl> `
  --released-external <pre-build copy of deploy\kb\external_w0.jsonl> `
  --released-manifest <pre-build copy of deploy\kb\v2\release_manifest.json> `
  --docs-diff <git diff --no-renames --name-only pinned..head> `
  --mode shadow
```

The released inputs must be snapshotted **before** the build, because
`release.py` promotes over them. `--no-renames` on the docs diff is not
cosmetic: rename detection reports only the new path, and a moved document is a
removal plus an addition in the corpus, so the removed half needs explaining too.

Two properties are structural rather than incidental:

- **It is its own process with its own exit code.** `release.py` calls
  `release_diff.main` and discards the return value, so a verdict living in that
  module would be a no-op — which is how this tree already shipped a gate that
  could not fail.
- **A missing input fails.** Every check reports what it read; one that could
  not read its evidence is `evidence_missing`, never a pass. The most recent
  empty gate here was `degradations`, where an asset report written before the
  key existed read as "nothing degraded".

`--mode shadow` (the default) evaluates everything, writes the verdict and always
exits 0. That is the only way to learn the base rate `--max-shrink` needs before
a threshold can block anything; `G7` therefore reports and does not block until
one is passed. `--mode enforce` exits 1 on any blocking failure.

Every check ships with a fixture that turns it red (`ReleaseGateTests`), and
`test_every_check_the_gate_emits_has_a_test_that_turns_it_red` fails when a new
check arrives without one — an assertion never observed failing is not evidence
of anything.

## Evaluation

Release quality is measured on 50 manually reviewed, RAG-relevant production
retrieval queries from 2026-06-26 through 2026-07-09. Raw chat text and local
judge caches stay under ignored `eval/.cache`; committed reports contain only
one-way case hashes, categories, retrieved chunk IDs, and aggregate grades.

The older 255-question synthetic set is retained only as a compatibility
regression. It must not be presented as evidence of current retrieval quality.

Fast deterministic rebuilds reuse locked VL and semantic outputs. Network link
checks, embedding refresh, real-chat reranking, and independent judging are
separate release checks and are expected to take longer than the build itself.
