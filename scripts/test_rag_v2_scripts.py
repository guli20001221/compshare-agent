import contextlib
from datetime import date
import io
import json
from pathlib import Path
import tempfile
import unittest
import unittest.mock
import zipfile

from scripts.rag_w0.build_corpus_embeddings import (
    compute_lf_sha256,
    sidecar_filename,
    write_sidecar,
)

from scripts.rag_v2 import build
from scripts.rag_v2 import pipeline
from scripts.rag_v2 import release
from scripts.rag_v2 import refresh_embeddings
from scripts.rag_v2 import release_diff
from scripts.rag_v2 import release_gate
from scripts.rag_v2 import release_inputs
from scripts.rag_v2.pipeline import (
    AssetNote,
    ModelVerseClient,
    SourceDocument,
    build_chunks,
    clean_public_text,
    document_type,
    inject_asset_notes,
    normalize_image_markup,
    merge_external,
    local_asset_candidates,
    plan_document_units,
    resolve_local_asset,
    semantic_parts,
    validate_chunks,
    vl_payload_answered,
    _canonical_remote_image_url,
    _image_content_type,
    _is_decorative_asset,
    _prepare_vl_image,
    _retryable_asset_failure,
    _question_patterns,
)


REPO_ROOT = Path(__file__).resolve().parents[1]


class _FakeResponse:
    """Minimal stand-in for the urlopen context manager json_chat consumes."""

    def __init__(self, payload):
        self._body = json.dumps({"choices": [{"message": {"content": json.dumps(payload, ensure_ascii=False)}}]})

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self):
        return self._body.encode("utf-8")


class RAGV2PipelineTests(unittest.TestCase):
    def test_embedding_sidecar_uses_float32_appropriate_precision(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "embeddings.jsonl"
            write_sidecar(
                path,
                corpus_digest="abc",
                embed_model="qwen3-embedding-8b",
                dim=2,
                rows=[("chunk", [0.12345678912345678, -0.000012345678912345])],
            )
            row = json.loads(path.read_text(encoding="utf-8").splitlines()[1])
            self.assertEqual([0.12345679, -1.2345679e-05], row["vector"])
            self.assertNotIn("123456781234", path.read_text(encoding="utf-8"))

    def test_question_patterns_are_document_metadata_and_never_invented(self):
        """question_patterns is scored as if the document had said it.

        compshare-kb joins the field into the BM25 patterns field AND into the
        text that is embedded and reranked, so an entry here is indistinguishable
        from source content at retrieval time. It may carry only what the
        document itself provides.
        """
        patterns = _question_patterns(
            "GPU 新功能发布记录", ["产品动态", "GPU 新功能发布记录"], "resource_purchase"
        )
        self.assertEqual(
            patterns,
            ["GPU 新功能发布记录", "产品动态 GPU 新功能发布记录", "resource purchase"],
            "only the title, the heading path, and the product area",
        )

        # No templated question forms. These were appended to every chunk and
        # match no real query, e.g. "怎么AttachCompshareDisk — 挂载已有云盘".
        for value in patterns:
            self.assertFalse(value.startswith("怎么"), f"invented question form: {value}")
            self.assertFalse(value.endswith("怎么办"), f"invented question form: {value}")

        # No hand-written topic phrasings. Three keyword rules used to inject
        # curated questions for disk billing, Coding Plan and resource capacity,
        # which made those three topics rank on wording their source text does
        # not support while every other topic in the corpus got none.
        billing = _question_patterns("磁盘计费", ["计费概览", "磁盘计费"], "billing_rule")
        for banned in ("系统盘100GB为什么还收费", "一直暂无资源是什么情况", "Coding Plan 支持退款吗"):
            self.assertNotIn(banned, billing)

    def test_question_patterns_takes_no_document_body(self):
        """The source_path/content arguments existed only to feed the deleted
        keyword rules. Removing them closes the seam for re-adding
        content-sniffing without touching the call site."""
        with self.assertRaises(TypeError):
            _question_patterns("t", ["h"], "area", "operation/upload.md", "body")

    def test_planner_error_is_not_locked_but_a_structural_fallback_is(self):
        """A null lock is permanent: it is read before the model on every build.

        So it must record a decision, not an accident. 28 of the 67 locks shipped
        with the current corpus are null with no reason attached, and a document
        that hit one transient planner error is indistinguishable from one that
        was deliberately left mechanical.
        """
        text = "\n\n".join(f"## 小节{i}\n" + "内容" * 400 for i in range(6))

        class _Boom:
            def __init__(self, cache_dir):
                self.cache_dir = cache_dir

            def json_chat(self, **kwargs):
                raise TimeoutError("planner timed out")

        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp) / "v2" / ".cache" / "modelverse"
            cache.mkdir(parents=True)
            locks = Path(tmp) / "v2" / "semantic_plans"
            pipeline.SEMANTIC_PLAN_STATS.clear()
            parts = semantic_parts(text, client=_Boom(cache), model="m")
            self.assertEqual(text.replace("\n\n", ""), "".join(parts).replace("\n\n", ""),
                             "the mechanical fallback must still be lossless")
            self.assertEqual([], list(locks.glob("*.json")),
                             "a transient planner error must not be locked in")
            self.assertEqual(1, pipeline.SEMANTIC_PLAN_STATS["planner_error:TimeoutError"])

        # A structural fallback IS a decision, so it locks — and says why.
        many = "\n\n".join(f"## 小节{i}\n" + "内容" * 200 for i in range(60))
        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp) / "v2" / ".cache" / "modelverse"
            cache.mkdir(parents=True)
            locks = Path(tmp) / "v2" / "semantic_plans"
            pipeline.SEMANTIC_PLAN_STATS.clear()
            semantic_parts(many, client=_Boom(cache), model="m")
            written = list(locks.glob("*.json"))
            self.assertEqual(1, len(written))
            self.assertEqual(
                {"groups": None, "reason": "too_many_blocks"},
                json.loads(written[0].read_text(encoding="utf-8")),
            )
            self.assertEqual(1, pipeline.SEMANTIC_PLAN_STATS["too_many_blocks"])

    def test_vl_non_answer_is_rejected_but_a_decorative_verdict_is_kept(self):
        """The schema gate passes a response that says nothing; this one does not.

        Rejecting must not catch a real "this image is decorative" verdict — that
        one arrives with a description and a confidence, and is the model doing
        its job.
        """
        answered = {
            "description": "云主机管理界面，显示主机状态与更多操作菜单。",
            "visible_text": ["运行中", "更多操作"], "controls": ["重启"], "relations": [],
            "confidence": 0.95, "visual_type": "界面截图", "include_in_rag": True,
        }
        decorative = {
            "description": "一个卡通风格的男性头像图标，棕色头发，穿蓝色衬衫。",
            "visible_text": [], "controls": [], "relations": [],
            "confidence": 0.92, "visual_type": "icon", "include_in_rag": False,
        }
        non_answer = {
            "description": "图片", "visible_text": [], "controls": [], "relations": [],
            "confidence": 0.0, "visual_type": "unknown", "include_in_rag": False,
        }
        # Zero confidence but the model did extract text: it looked, so keep it.
        text_only = {
            "description": "", "visible_text": ["nvidia-smi"], "controls": [], "relations": [],
            "confidence": 0.0, "visual_type": "unknown", "include_in_rag": True,
        }
        self.assertTrue(vl_payload_answered(answered))
        self.assertTrue(vl_payload_answered(decorative))
        self.assertTrue(vl_payload_answered(text_only))
        self.assertFalse(vl_payload_answered(non_answer))
        self.assertFalse(vl_payload_answered({**non_answer, "confidence": None}))
        self.assertFalse(vl_payload_answered({**non_answer, "visible_text": ["", "  "]}))

    def test_rejected_response_is_retried_and_never_cached(self):
        """One bad minute must not become a permanent cache entry.

        json_chat used to write whatever came back. With `accept`, a non-answer
        is retried; if the retry answers, that is what gets cached, and if every
        attempt fails the payload is returned without a cache write so the next
        build asks again.
        """
        non_answer = {"description": "图片", "visible_text": [], "confidence": 0.0}
        good = {"description": "终端输出", "visible_text": ["nvidia-smi"], "confidence": 0.9}

        with tempfile.TemporaryDirectory() as tmp:
            client = ModelVerseClient(base_url="https://example.invalid/v1", api_key="k", cache_dir=Path(tmp))
            responses = [non_answer, non_answer, good]
            calls = []

            def fake_urlopen(request, timeout=None):
                calls.append(1)
                return _FakeResponse(responses[len(calls) - 1])

            with unittest.mock.patch.object(pipeline, "urlopen", fake_urlopen), \
                 unittest.mock.patch.object(pipeline.time, "sleep", lambda _s: None):
                got = client.json_chat(model="m", prompt="p", accept=vl_payload_answered)
            self.assertEqual(good, got)
            self.assertEqual(3, len(calls), "should have retried past both non-answers")
            self.assertEqual(good, client.cached_json(model="m", prompt="p"))

        with tempfile.TemporaryDirectory() as tmp:
            client = ModelVerseClient(base_url="https://example.invalid/v1", api_key="k", cache_dir=Path(tmp))
            calls = []

            def always_bad(request, timeout=None):
                calls.append(1)
                return _FakeResponse(non_answer)

            with unittest.mock.patch.object(pipeline, "urlopen", always_bad), \
                 unittest.mock.patch.object(pipeline.time, "sleep", lambda _s: None):
                got = client.json_chat(model="m", prompt="p", retries=2, accept=vl_payload_answered)
            self.assertEqual(non_answer, got)
            self.assertEqual(3, len(calls))
            self.assertIsNone(client.cached_json(model="m", prompt="p"),
                              "a non-answer must not be cached")

    def test_site_root_image_reference_resolves_under_public(self):
        """A leading slash is the site root, which Next.js serves from public/.

        compshare-docs wrote these as literal repo paths (public/x.jpg) until the
        App Router migration rewrote them to /x.jpg. Both forms have to resolve,
        or 14 required images in content/operation/ fail the build.
        """
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "public").mkdir()
            (root / "public" / "sysdisk_step1.jpg").write_bytes(b"jpg")
            path = root / "content" / "operation" / "gpu" / "disk.md"
            path.parent.mkdir(parents=True)
            path.write_text("![](/sysdisk_step1.jpg)", encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="content/operation/gpu/disk.md",
                source_kind="platform_public_doc", source_origin="official", title="disk",
                text=path.read_text(encoding="utf-8"), surface_url=None, root=root, absolute_path=path,
            )
            self.assertEqual(
                (root / "public" / "sysdisk_step1.jpg").resolve(),
                resolve_local_asset(doc, "/sysdisk_step1.jpg"),
            )
            # The pre-migration spelling still resolves, so a rebuild of an older
            # revision does not regress.
            self.assertEqual(
                (root / "public" / "sysdisk_step1.jpg").resolve(),
                resolve_local_asset(doc, "public/sysdisk_step1.jpg"),
            )
            self.assertIsNone(resolve_local_asset(doc, "/not_there.jpg"))

    def test_site_root_reference_is_never_resolved_against_the_document_directory(self):
        """Windows discards the base when joining an absolute-looking path.

        Path("F:/a/b") / "/c.png" is "/c.png", which resolves against the current
        drive. Left in the candidate list, a site-root reference could silently
        match an unrelated file and feed its VL description into the corpus as
        the documented screenshot.

        Asserted on the candidate list, not on the return value: the two differ
        only when a file exists at the drive root, and a test must not create one
        — checking the outcome here would pass with or without the guard.
        """
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            docs_dir = root / "content" / "operation"
            docs_dir.mkdir(parents=True)
            path = docs_dir / "disk.md"
            path.write_text("![](/shot.png)", encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="content/operation/disk.md",
                source_kind="platform_public_doc", source_origin="official", title="disk",
                text=path.read_text(encoding="utf-8"), surface_url=None, root=root, absolute_path=path,
            )
            site_root = local_asset_candidates(doc, "/shot.png")
            self.assertEqual([root / "shot.png", root / "public" / "shot.png"], site_root)
            self.assertNotIn(docs_dir.resolve(), [c.parent for c in site_root])
            # A genuinely document-relative reference still starts at the document.
            self.assertEqual(
                (docs_dir / "shot.png").resolve(),
                local_asset_candidates(doc, "shot.png")[0],
            )

    def test_cleaning_preserves_public_examples_and_does_not_redact(self):
        raw = "---\ntitle: x\n---\n# API\n\nAPI Key：sk-public-example\n\nhttps://cp.compshare.cn/v1\n"
        cleaned = clean_public_text(raw)
        self.assertIn("sk-public-example", cleaned)
        self.assertIn("https://cp.compshare.cn/v1", cleaned)
        self.assertNotIn("REDACTED", cleaned)

    def test_heading_chunking_preserves_complete_short_sections(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "faq.md"
            path.write_text("# 登录\n\n使用 root 和 23 端口。\n\n## 忘记密码\n\n在控制台重置密码。", encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="faq.md", source_kind="public_faq_export",
                source_origin="official", title="登录", text=path.read_text(encoding="utf-8"),
                surface_url=None, root=root, absolute_path=path,
            )
            rows, stats = build_chunks(
                [doc], kb_version="kb.platform.v2.test", valid_from="2026-07-15",
                asset_notes={}, semantic_client=None, semantic_model="qwen3.7-max",
            )
            self.assertEqual(2, stats["chunk_count"])
            self.assertTrue(any("控制台重置密码" in row["content"] for row in rows))
            self.assertTrue(all("retrieval_text" not in row for row in rows))
            self.assertEqual([], validate_chunks(rows, expected_version="kb.platform.v2.test"))

    def test_long_text_falls_back_without_loss(self):
        text = "\n\n".join(f"段落{i} " + "内容" * 300 for i in range(20))
        parts = semantic_parts(text, client=None, model="unused")
        self.assertTrue(all(len(part) <= 4000 for part in parts))
        self.assertEqual(text.replace("\n\n", ""), "".join(parts).replace("\n\n", ""))

    def test_short_api_document_is_exactly_one_chunk(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "public" / "action_md" / "Create.md"
            path.parent.mkdir(parents=True)
            text = "# Create\n\n# Request Parameters\n字段 A\n\n# Response Elements\n字段 B"
            path.write_text(text, encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="public/action_md/Create.md", source_kind="platform_public_doc",
                source_origin="official", title="Create", text=text, surface_url=None, root=root, absolute_path=path,
            )
            rows, _ = build_chunks(
                [doc], kb_version="kb.platform.v2.test", valid_from="2026-07-15",
                asset_notes={}, semantic_client=None, semantic_model="qwen3.7-max",
            )
            self.assertEqual("api_reference", document_type(doc))
            self.assertEqual(1, len(rows))
            self.assertEqual("complete_document", rows[0]["chunk_role"])
            self.assertIn("Request Parameters", rows[0]["content"])
            self.assertIn("Response Elements", rows[0]["content"])

    def test_short_operation_guide_is_exactly_one_chunk(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "pages" / "operation" / "login.md"
            path.parent.mkdir(parents=True)
            text = "# 登录指南\n\n## 步骤一\n打开控制台。\n\n## 步骤二\n复制命令。"
            path.write_text(text, encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="pages/operation/login.md", source_kind="platform_public_doc",
                source_origin="official", title="登录指南", text=text, surface_url=None, root=root, absolute_path=path,
            )
            units = plan_document_units(doc, text)
            self.assertEqual(1, len(units))
            self.assertEqual("complete_document", units[0][2])

    def test_oversized_guide_splits_only_between_complete_sections(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "pages" / "operation" / "long.md"
            path.parent.mkdir(parents=True)
            text = "# 长指南\n\n## 完整步骤一\n" + "甲" * 2500 + "\n\n## 完整步骤二\n" + "乙" * 2500
            path.write_text(text, encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="pages/operation/long.md", source_kind="platform_public_doc",
                source_origin="official", title="长指南", text=text, surface_url=None, root=root, absolute_path=path,
            )
            units = plan_document_units(doc, text)
            self.assertEqual(2, len(units))
            self.assertTrue(all(unit[2] == "complete_step_group" for unit in units))
            self.assertTrue(all(len(unit[1]) <= 4000 for unit in units))

    def test_legacy_external_is_retained_without_duplicate_content(self):
        legacy = [{"chunk_id": "old", "content": "same"}]
        rebuilt = [{"chunk_id": "new", "content": "same"}, {"chunk_id": "new2", "content": "different"}]
        merged, skipped = merge_external(legacy, rebuilt)
        self.assertEqual(["new2", "old"], [row["chunk_id"] for row in merged])
        self.assertEqual(1, skipped)

    def test_images_become_caption_only_without_runtime_links(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "faq.md"
            path.write_text("![控制台](screen.png)", encoding="utf-8")
            doc = SourceDocument(
                source_id="test", source_path="faq.md", source_kind="public_faq_export",
                source_origin="official", title="FAQ", text=path.read_text(encoding="utf-8"),
                surface_url=None, root=root, absolute_path=path,
            )
            note = AssetNote(
                asset_id="asset-1", source_path="faq.md", repo_path="unused.png",
                public_url="https://example.com/unused.png", description="创建实例页面",
                visible_text=["创建"], controls=["确认"], relations=["确认按钮位于底部"],
                confidence=0.98, model="vl", visual_type="ui",
            )
            content, media = inject_asset_notes(doc, {("test", "faq.md", "screen.png"): note})
            self.assertIn("[图片说明] 创建实例页面", content)
            self.assertIn("[图片文字] 创建", content)
            self.assertIn("[界面控件] 确认", content)
            self.assertNotIn("http", content)
            self.assertNotIn("查看原图", content)
            self.assertEqual([], media)

    def test_missing_external_image_does_not_pollute_runtime_text(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "guide.md"
            path.write_text("操作说明\n\n![工作流截图](missing.png)\n\n下一步说明", encoding="utf-8")
            doc = SourceDocument(
                source_id="external-test", source_path="guide.md", source_kind="external_doc",
                source_origin="external_official", title="指南", text=path.read_text(encoding="utf-8"),
                surface_url=None, root=root, absolute_path=path,
            )
            content, media = inject_asset_notes(doc, {})
            self.assertEqual("操作说明\n\n\n\n下一步说明", content)
            self.assertNotIn("原文图片未包含", content)
            self.assertEqual([], media)

    VL_PAYLOAD = {
        "description": "新截图", "visible_text": ["创建实例"], "controls": ["确认"],
        "relations": [], "confidence": 0.9, "visual_type": "ui", "include_in_rag": True,
    }

    def _caption_lock_fixture(self, tmp: Path, doc_path: str, image_bytes: bytes = b"pretend-png-bytes"):
        """A document with one local image, plus the release dir describe_assets uses."""
        root = tmp / "docs"
        (root / Path(doc_path).parent).mkdir(parents=True, exist_ok=True)
        image = root / "screen.png"
        image.write_bytes(b"\x89PNG\r\n\x1a\n" + image_bytes)
        page = root / doc_path
        page.write_text("说明\n\n![控制台](/screen.png)\n", encoding="utf-8")
        doc = SourceDocument(
            source_id="gitlab-compshare-docs", source_path=doc_path,
            source_kind="public_faq_export", source_origin="official", title="页面",
            text=page.read_text(encoding="utf-8"), surface_url=None, root=root, absolute_path=page,
        )
        return doc, image

    def _remote_doc(self, tmp: Path, url: str, *, source_origin: str = "official", body: str | None = None):
        root = tmp / "docs"
        root.mkdir(parents=True, exist_ok=True)
        page = root / "remote.md"
        page.write_text(body if body is not None else f"说明\n\n![截图]({url})\n", encoding="utf-8")
        return SourceDocument(
            source_id="gitlab-compshare-docs", source_path="remote.md",
            source_kind="public_faq_export", source_origin=source_origin, title="页面",
            text=page.read_text(encoding="utf-8"), surface_url=None, root=root, absolute_path=page,
        )

    def _note(self, asset_id: str, *, source_path: str, public_url: str = "",
              description: str = "控制台创建实例页面", model: str = "vl-model"):
        return AssetNote(
            asset_id=asset_id, source_path=source_path, repo_path=None, public_url=public_url,
            description=description, visible_text=["创建实例"], controls=["确认"], relations=[],
            confidence=0.98, model=model, visual_type="ui",
        )

    def _write_lock(self, release: Path, notes, *, contract=None, failures=()):
        (release / "asset_lock.json").write_text(json.dumps({
            "schema_version": pipeline.ASSET_LOCK_SCHEMA_VERSION,
            "contract": pipeline.caption_contract_digest() if contract is None else contract,
            "notes": list(notes),
            "failures": list(failures),
        }, ensure_ascii=False), encoding="utf-8")

    def _locked_row(self, note: AssetNote, ref: str, source_id: str = "gitlab-compshare-docs"):
        return {"source_id": source_id, "ref": ref, **vars(note)}

    def _mock_client(self, release: Path, payload=None):
        """A VL client that either answers, or fails the test for being called."""
        client = unittest.mock.Mock(spec=ModelVerseClient)
        client.cache_dir = release / ".cache" / "modelverse"
        if payload is None:
            def explode(*args, **kwargs):
                raise AssertionError("describe_assets called the VL model for an unchanged image")
            client.json_chat.side_effect = explode
            client.cached_json.side_effect = explode
        else:
            client.cached_json.return_value = None
            client.json_chat.return_value = dict(payload)
        return client

    def _fake_remote(self, digest_by_url: dict, calls: list | None = None):
        def fetch(url, cache_dir):
            if calls is not None:
                calls.append(url)
            if url not in digest_by_url:
                raise AssertionError(f"describe_assets re-fetched {url}")
            digest = digest_by_url[url]
            cache_dir.mkdir(parents=True, exist_ok=True)
            path = cache_dir / f"{digest[:24]}.png"
            if not path.exists():
                path.write_bytes(b"\x89PNG\r\n\x1a\n" + digest.encode("ascii"))
            return pipeline.RemoteAsset(path=path, digest=digest, content_type="image/png")
        return fetch

    def _describe(self, release: Path, docs, client, **kwargs):
        """describe_assets, dropping degradations for the tests that ignore them."""
        notes, failures, _degradations = self._describe_full(release, docs, client, **kwargs)
        return notes, failures

    def _describe_full(self, release: Path, docs, client, **kwargs):
        return pipeline.describe_assets(
            docs, client=client, model="vl-model", fallback_model=None,
            assets_dir=release / "assets", raw_asset_base_url="https://example.invalid/a", **kwargs,
        )

    def test_a_moved_document_reuses_its_caption_instead_of_recaptioning(self):
        """The regression that made every routine update a full re-caption.

        Reuse used to be keyed on (source_id, source_path, ref) and gated behind
        a fingerprint covering every image reference in the corpus. Both are
        invalidated by the ordinary act of updating the docs, so a rebuild
        re-captioned all ~1900 images -- paying for them again and, at the
        concurrency the build used, silently losing some to non-answers.
        """
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _original, image = self._caption_lock_fixture(Path(tmp), "pages/guide.md")
            note = self._note("asset-" + pipeline.sha256_file(image)[:16], source_path="pages/guide.md")
            self._write_lock(release, [self._locked_row(note, "/screen.png")])

            # Same image bytes, new path: exactly what the pages/ -> content/
            # App Router migration did to every document in the corpus.
            moved, _image = self._caption_lock_fixture(Path(tmp), "content/guide.md")
            client = self._mock_client(release)

            notes, failures = self._describe(release, [moved], client)

            self.assertEqual([], failures)
            reused = notes[("gitlab-compshare-docs", "content/guide.md", "/screen.png")]
            self.assertEqual("控制台创建实例页面", reused.description)
            self.assertEqual(["创建实例"], reused.visible_text)
            # The note follows the image to its new home rather than keeping the
            # path it was first captioned under.
            self.assertEqual("content/guide.md", reused.source_path)

    def test_a_local_image_replaced_in_place_is_recaptioned(self):
        """Same document, same markdown reference, different bytes.

        Reuse was consulted by (source_id, source_path, ref) BEFORE the content
        key and never checked that the stored note's asset_id matched the file on
        disk, so swapping a screenshot kept the caption written for the one it
        replaced. The stale note was then re-serialized under its dead asset_id,
        so every later build hit the same path key again.
        """
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _doc, image = self._caption_lock_fixture(Path(tmp), "guide.md", b"old-bytes")
            old_digest = pipeline.sha256_file(image)
            self._write_lock(release, [self._locked_row(
                self._note("asset-" + old_digest[:16], source_path="guide.md", description="旧截图"),
                "/screen.png")])

            replaced, image = self._caption_lock_fixture(Path(tmp), "guide.md", b"different-bytes")
            new_digest = pipeline.sha256_file(image)
            self.assertNotEqual(old_digest, new_digest)
            client = self._mock_client(release, payload=self.VL_PAYLOAD)

            notes, failures = self._describe(release, [replaced], client)

            self.assertEqual([], failures)
            self.assertEqual(1, client.json_chat.call_count)
            note = notes[("gitlab-compshare-docs", "guide.md", "/screen.png")]
            self.assertEqual("新截图", note.description)
            self.assertEqual("asset-" + new_digest[:16], note.asset_id)

    def test_a_remote_image_replaced_behind_a_stable_url_is_recaptioned(self):
        """1200 of the corpus's 1276 distinct images are reached by URL.

        Reuse was keyed on the URL string and short-circuited before any
        download, so an image swapped behind a stable URL was never re-fetched,
        re-hashed or re-captioned. For the 223 URLs whose stored verdict is
        include_in_rag=false that silently deleted the replacement's content from
        the corpus, because such a note renders as the empty string.
        """
        url = "https://docs.invalid/a.png"
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [self._locked_row(
                self._note("remote-" + "a" * 16, source_path="remote.md", public_url=url,
                           description="旧二维码"),
                url)])
            doc = self._remote_doc(Path(tmp), url)
            client = self._mock_client(release, payload=self.VL_PAYLOAD)

            notes, failures = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({url: "b" * 64}))

            self.assertEqual([], failures)
            self.assertEqual(1, client.json_chat.call_count)
            note = notes[("gitlab-compshare-docs", "remote.md", url)]
            self.assertEqual("新截图", note.description)
            self.assertEqual("remote-" + "b" * 16, note.asset_id)

    def test_a_remote_image_whose_bytes_are_unchanged_is_reused(self):
        """The saving the revalidation pass exists to keep.

        A conditional GET that comes back 304 yields the digest we already hold,
        which is the same reuse key the stored note carries -- so the image costs
        one validator round-trip instead of one model call.
        """
        url = "https://docs.invalid/a.png"
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [self._locked_row(
                self._note("remote-" + "a" * 16, source_path="remote.md", public_url=url), url)])
            doc = self._remote_doc(Path(tmp), url)
            client = self._mock_client(release)
            fetched: list = []

            notes, failures = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({url: "a" * 64}, fetched))

            self.assertEqual([], failures)
            self.assertEqual([url], fetched)
            self.assertEqual("控制台创建实例页面", notes[("gitlab-compshare-docs", "remote.md", url)].description)

    def test_a_caption_contract_change_invalidates_every_locked_caption(self):
        """Editing the prompt used to invalidate nothing at all.

        The only per-note guard was the model name, and the "caption-only-v3"
        tag that was meant to be the escape hatch reached only the corpus-wide
        fingerprint -- never the per-image reuse map.
        """
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _doc, image = self._caption_lock_fixture(Path(tmp), "guide.md")
            self._write_lock(
                release,
                [self._locked_row(self._note("asset-" + pipeline.sha256_file(image)[:16],
                                             source_path="guide.md"), "/screen.png")],
                contract="the-digest-of-a-different-prompt",
            )
            doc, _image = self._caption_lock_fixture(Path(tmp), "guide.md")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)

            notes, failures = self._describe(release, [doc], client)

            self.assertEqual([], failures)
            self.assertEqual(1, client.json_chat.call_count)
            self.assertEqual("新截图", notes[("gitlab-compshare-docs", "guide.md", "/screen.png")].description)

    def test_an_image_removed_from_the_docs_leaves_the_lock(self):
        """The lock records this build's captions, not every caption ever taken."""
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _doc, image = self._caption_lock_fixture(Path(tmp), "guide.md")
            self._write_lock(release, [self._locked_row(
                self._note("asset-" + pipeline.sha256_file(image)[:16], source_path="guide.md"),
                "/screen.png")])
            without = self._remote_doc(Path(tmp), "", body="说明\n\n正文里已经没有图片了。\n")
            client = self._mock_client(release)

            notes, failures = self._describe(release, [without], client)

            self.assertEqual({}, notes)
            self.assertEqual([], failures)
            rewritten = json.loads((release / "asset_lock.json").read_text(encoding="utf-8"))
            self.assertEqual([], rewritten["notes"])
            self.assertEqual(pipeline.caption_contract_digest(), rewritten["contract"])

    def test_a_permanent_remote_failure_is_not_refetched_for_third_party_docs(self):
        """52 dead URLs at three attempts against a 10s timeout, every build."""
        url = "https://docs.invalid/gone.png"
        failure = {"source_id": "external-comfyui", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404",
                   "severity": "warning"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure])
            doc = self._remote_doc(Path(tmp), url, source_origin="external_comfyui")
            client = self._mock_client(release)
            fetched: list = []

            notes, failures_out = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({}, fetched))

            self.assertEqual({}, notes)
            # The point of the carry is the round-trip that never happens.
            self.assertEqual([], fetched)
            self.assertEqual(1, len(failures_out))
            self.assertEqual("warning", failures_out[0]["severity"])
            self.assertIn("HTTP Error 404", failures_out[0]["reason"])

    def test_a_platform_image_that_fails_again_blocks_instead_of_being_downgraded(self):
        """The carried copy is always a warning; this build's verdict is not.

        Reusing the stored failure for an image we did re-attempt would turn an
        internal image's error -- which build.py raises on -- into a warning the
        release sails past.
        """
        url = "https://docs.invalid/gone.png"
        failure = {"source_id": "gitlab-compshare-docs", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404",
                   "severity": "warning"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure])
            doc = self._remote_doc(Path(tmp), url, source_origin="official")
            client = self._mock_client(release)

            notes, failures_out = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({}))

            self.assertEqual({}, notes)
            self.assertEqual(1, len(failures_out))
            self.assertEqual("error", failures_out[0]["severity"])

    def test_a_permanent_remote_failure_is_still_retried_for_platform_docs(self):
        """An internal image is mandatory: the release gate has to block on it."""
        url = "https://docs.invalid/gone.png"
        failure = {"source_id": "gitlab-compshare-docs", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404",
                   "severity": "error"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure])
            doc = self._remote_doc(Path(tmp), url, source_origin="official")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)
            fetched: list = []

            notes, failures_out = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({url: "c" * 64}, fetched))

            self.assertEqual([url], fetched)
            self.assertEqual([], failures_out)
            self.assertEqual("remote-" + "c" * 16, notes[("gitlab-compshare-docs", "remote.md", url)].asset_id)

    def test_preprocessing_changes_force_a_caption_contract_decision(self):
        """VL_PREPROCESS_VERSION is a human decision; this makes it a required one."""
        self.assertEqual(
            pipeline.PREPROCESS_SOURCE_PINS,
            pipeline.preprocess_source_digests(),
            msg="A function the caption contract stands for changed. Bump "
                "VL_PREPROCESS_VERSION so every stored caption is re-earned, or "
                "re-pin here if the edit was purely cosmetic.",
        )

    def test_only_content_addressed_notes_are_reusable(self):
        """Identity says the bytes match, not that the caption is ours."""
        local = self._note("asset-0123456789abcdef", source_path="a.md")
        self.assertEqual("asset-0123456789abcdef", pipeline.locked_note_identity(local))
        remote = self._note("remote-0123456789abcdef", source_path="a.md",
                            public_url="https://x.invalid/a.png")
        self.assertEqual("remote-0123456789abcdef", pipeline.locked_note_identity(remote))
        # A decorative note digests the reference string, not any content, so
        # matching on it would let a lookalike reference claim a verdict about
        # bytes nobody read -- and re-deriving one costs no model call.
        decorative = self._note("decorative-0123456789abcdef", source_path="a.md")
        self.assertIsNone(pipeline.locked_note_identity(decorative))
        opaque = self._note("legacy", source_path="a.md")
        self.assertIsNone(pipeline.locked_note_identity(opaque))

    def test_the_shipped_lock_carries_the_contract_it_was_produced_under(self):
        """The backfill is only honest if it still matches the code."""
        lock = json.loads((REPO_ROOT / "deploy/kb/v2/asset_lock.json").read_text(encoding="utf-8"))
        self.assertEqual(pipeline.ASSET_LOCK_SCHEMA_VERSION, lock["schema_version"])
        self.assertEqual(pipeline.caption_contract_digest(), lock["contract"])
        self.assertNotIn("fingerprint", lock)
        # Every note must still load, or the build silently re-captions the lot.
        for item in lock["notes"]:
            AssetNote(**{k: v for k, v in item.items() if k not in {"source_id", "ref"}})

    def test_a_note_produced_by_another_model_is_not_reused(self):
        """Why models are allowed to stay out of the contract digest.

        The digest covers the prompt and the preprocessing; the model is checked
        per note instead, so swapping the fallback model re-earns the handful of
        captions it produced rather than all ~1300.
        """
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _doc, image = self._caption_lock_fixture(Path(tmp), "guide.md")
            self._write_lock(release, [self._locked_row(
                self._note("asset-" + pipeline.sha256_file(image)[:16], source_path="guide.md",
                           description="别的模型写的", model="some-other-vl-model"),
                "/screen.png")])
            doc, _image = self._caption_lock_fixture(Path(tmp), "guide.md")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)

            notes, failures = self._describe(release, [doc], client)

            self.assertEqual([], failures)
            self.assertEqual(1, client.json_chat.call_count)
            self.assertEqual("vl-model", notes[("gitlab-compshare-docs", "guide.md", "/screen.png")].model)

    def test_a_transient_remote_failure_is_re_attempted_even_for_third_party_docs(self):
        """Only permanent verdicts may be carried; a timeout is not a verdict."""
        url = "https://docs.invalid/flaky.png"
        failure = {"source_id": "external-comfyui", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:TimeoutError:timed out",
                   "severity": "warning"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure])
            doc = self._remote_doc(Path(tmp), url, source_origin="external_comfyui")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)
            fetched: list = []

            _notes, failures_out = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({url: "d" * 64}, fetched))

            self.assertEqual([url], fetched)
            self.assertEqual([], failures_out)

    def test_a_url_a_platform_doc_also_uses_is_re_attempted(self):
        """external_only is an AND over every document that references the URL.

        A third-party snapshot must not be able to decide, on a platform
        document's behalf, that an image it needs may stay unresolved.
        """
        url = "https://docs.invalid/shared.png"
        failure = {"source_id": "external-comfyui", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404",
                   "severity": "warning"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure])
            external = self._remote_doc(Path(tmp), url, source_origin="external_comfyui")
            internal = self._remote_doc(Path(tmp), url, source_origin="official")
            # The external document is listed LAST, so a last-wins fold would
            # decide the URL is skippable.
            client = self._mock_client(release)
            fetched: list = []

            _notes, failures_out = self._describe(
                release, [internal, external], client, fetch_remote=self._fake_remote({}, fetched))

            self.assertEqual([url], fetched)
            self.assertIn("error", {item["severity"] for item in failures_out})

    def test_a_contract_mismatch_drops_carried_failures_too(self):
        """"Every caption will be re-earned" has to include the verdicts."""
        url = "https://docs.invalid/gone.png"
        failure = {"source_id": "external-comfyui", "source": "remote.md", "ref": url,
                   "reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404",
                   "severity": "warning"}
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [], failures=[failure], contract="a-different-contract")
            doc = self._remote_doc(Path(tmp), url, source_origin="external_comfyui")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)
            fetched: list = []

            _notes, _failures = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({url: "e" * 64}, fetched))

            self.assertEqual([url], fetched)

    def test_an_unreadable_lock_reuses_nothing(self):
        """AssetNote(**...) raises on field drift, and that used to be silent."""
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            _doc, image = self._caption_lock_fixture(Path(tmp), "guide.md")
            good = self._locked_row(
                self._note("asset-" + pipeline.sha256_file(image)[:16], source_path="guide.md"),
                "/screen.png")
            # A readable note BEFORE the unreadable one: the partially-filled
            # reuse map has to be discarded too, not just the notes dict.
            row = dict(good)
            row["a_field_from_a_future_schema"] = 1
            self._write_lock(release, [good, row])
            doc, _image = self._caption_lock_fixture(Path(tmp), "guide.md")
            client = self._mock_client(release, payload=self.VL_PAYLOAD)

            notes, failures = self._describe(release, [doc], client)

            self.assertEqual([], failures)
            self.assertEqual(1, client.json_chat.call_count)
            self.assertEqual("新截图", notes[("gitlab-compshare-docs", "guide.md", "/screen.png")].description)

    def test_a_decorative_remote_reference_is_never_fetched(self):
        """The deterministic filter runs before the network, not after it."""
        badge = "https://img.shields.io/badge/build-passing.svg"
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [])
            doc = self._remote_doc(Path(tmp), badge, body=f"说明\n\n![badge]({badge})\n")
            client = self._mock_client(release)
            fetched: list = []

            notes, failures = self._describe(
                release, [doc], client, fetch_remote=self._fake_remote({}, fetched))

            self.assertEqual([], fetched)
            self.assertEqual([], failures)
            note = notes[("gitlab-compshare-docs", "remote.md", badge)]
            self.assertTrue(note.asset_id.startswith("decorative-"))
            self.assertFalse(note.include_in_rag)

    def _serve(self, handler):
        import http.server
        import threading

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_address[1]}/a.png"

    def test_resolve_remote_image_revalidates_instead_of_re_downloading(self):
        """The whole reason a content key is affordable for 1200 remote images."""
        import http.server

        body = b"\x89PNG\r\n\x1a\n" + b"real-bytes"
        seen: list = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append(self.headers.get("If-None-Match"))
                if self.headers.get("If-None-Match") == '"v1"':
                    self.send_response(304)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "image/png")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("ETag", '"v1"')
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        url = self._serve(Handler)
        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp) / "remote-assets"
            first = pipeline.resolve_remote_image(url, cache)
            self.assertEqual(pipeline.sha256_bytes(body), first.digest)
            self.assertTrue(first.path.is_file())
            stored = json.loads(next((cache / "by-url").glob("*.json")).read_text(encoding="utf-8"))
            self.assertEqual('"v1"', stored["etag"])
            self.assertEqual(pipeline.sha256_bytes(body), stored["digest"])

            second = pipeline.resolve_remote_image(url, cache)
            self.assertEqual(first.digest, second.digest)
            # First request unconditional, second carried the validator.
            self.assertEqual([None, '"v1"'], seen)

    def test_resolve_remote_image_keeps_cached_bytes_when_the_origin_is_unreachable(self):
        """A build must not lose a caption because a CDN blinked.

        The bytes on disk are exactly what the stored caption describes, and the
        lock is rewritten from this build's notes -- so raising here would both
        fail the build and delete the caption it could have kept.
        """
        url = "https://docs.invalid/a.png"
        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp) / "remote-assets"
            (cache / "by-url").mkdir(parents=True)
            body = b"\x89PNG\r\n\x1a\n" + b"cached-bytes"
            digest = pipeline.sha256_bytes(body)
            (cache / f"{digest[:24]}.png").write_bytes(body)
            (cache / "by-url" / f"{pipeline.sha256_bytes(url.encode('utf-8'))}.json").write_text(
                json.dumps({"content_type": "image/png", "digest": digest, "etag": '"v1"',
                            "filename": f"{digest[:24]}.png", "last_modified": ""}),
                encoding="utf-8")

            with unittest.mock.patch.object(pipeline, "urlopen", side_effect=OSError("unreachable")), \
                 unittest.mock.patch.object(pipeline.time, "sleep"):
                asset = pipeline.resolve_remote_image(url, cache)

            self.assertEqual(digest, asset.digest)
            self.assertEqual(body, asset.path.read_bytes())

    def test_resolve_remote_image_refuses_a_truncated_body(self):
        """A short read must never be blessed with the server's ETag.

        Storing a validator beside bytes nobody verified makes the truncation
        permanent: every later build revalidates to 304 and keeps it.
        """
        import http.server

        body = b"\x89PNG\r\n\x1a\n" + b"half"

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "image/png")
                self.send_header("Content-Length", str(len(body) + 64))
                self.send_header("ETag", '"v1"')
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        url = self._serve(Handler)
        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp) / "remote-assets"
            with unittest.mock.patch.object(pipeline.time, "sleep"):
                with self.assertRaises(Exception):
                    pipeline.resolve_remote_image(url, cache)
            self.assertEqual([], list((cache / "by-url").glob("*.json")))

    def test_the_pinned_pillow_version_is_the_one_the_contract_attests_to(self):
        """One producer for the number, and the file pip reads must agree with it.

        Pillow renders the contact sheet the model is shown, so two versions are
        two instructions. Declaring it without folding it into the digest would
        let two machines write captions the contract calls interchangeable.
        """
        requirements = (REPO_ROOT / "scripts/rag_v2/requirements.txt").read_text(encoding="utf-8")
        self.assertIn(f"pillow=={pipeline.VL_PILLOW_VERSION}", requirements)
        with unittest.mock.patch.object(pipeline, "VL_PILLOW_VERSION", "0.0.0"):
            self.assertNotEqual(pipeline.caption_contract_digest(),
                                json.loads((REPO_ROOT / "deploy/kb/v2/asset_lock.json")
                                           .read_text(encoding="utf-8"))["contract"])

    def test_an_unpinned_pillow_refuses_to_write_a_caption(self):
        """A build that cannot honour the preprocessing it attests to must stop."""
        try:
            from PIL import Image
        except ImportError:
            self.skipTest("Pillow is not installed")
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "anim.gif"
            frames = [Image.new("RGB", (4, 4), color) for color in ("red", "blue")]
            frames[0].save(source, save_all=True, append_images=frames[1:], duration=10)
            with unittest.mock.patch.object(pipeline, "VL_PILLOW_VERSION", "0.0.0"):
                with self.assertRaises(RuntimeError) as caught:
                    pipeline._prepare_vl_image(source, Path(tmp) / "vl-ready")
            self.assertIn("0.0.0", str(caught.exception))

            # The pin binds a RENDERING, so it may only be enforced where one
            # happens. A non-image and a SINGLE-frame GIF are both handed to the
            # model untouched; failing those would fail builds over a transform
            # that never runs.
            plain = Path(tmp) / "shot.png"
            plain.write_bytes(b"\x89PNG\r\n\x1a\n")
            static_gif = Path(tmp) / "static.gif"
            Image.new("RGB", (4, 4), "red").save(static_gif)
            with unittest.mock.patch.object(pipeline, "VL_PILLOW_VERSION", "0.0.0"):
                self.assertEqual(plain, pipeline._prepare_vl_image(plain, Path(tmp) / "vl-ready"))
                self.assertEqual(static_gif,
                                 pipeline._prepare_vl_image(static_gif, Path(tmp) / "vl-ready"))

    def test_an_unrevalidated_platform_image_is_reported_as_a_degradation(self):
        """Availability-first is a policy; hiding that it fired is not.

        The build keeps the cached bytes rather than dying on a CDN blip, but
        the caption then describes bytes nothing checked this run, and for a
        platform image that has to reach the report and the release gate.
        """
        url = "https://docs.invalid/a.png"
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [self._locked_row(
                self._note("remote-" + "a" * 16, source_path="remote.md", public_url=url), url)])

            def stale_fetch(fetch_url, cache_dir):
                cache_dir.mkdir(parents=True, exist_ok=True)
                path = cache_dir / "cached.png"
                path.write_bytes(b"\x89PNG\r\n\x1a\n")
                return pipeline.RemoteAsset(path=path, digest="a" * 64,
                                            content_type="image/png", stale=True)

            platform = self._remote_doc(Path(tmp), url, source_origin="official")
            _n, _f, degradations = self._describe_full(
                release, [platform], self._mock_client(release), fetch_remote=stale_fetch)
            self.assertEqual(1, len(degradations))
            self.assertEqual("error", degradations[0]["severity"])
            self.assertIn("remote_revalidation_failed", degradations[0]["reason"])

            third_party = self._remote_doc(Path(tmp), url, source_origin="external_comfyui")
            _n, _f, degradations = self._describe_full(
                release, [third_party], self._mock_client(release), fetch_remote=stale_fetch)
            self.assertEqual("warning", degradations[0]["severity"])

    def test_a_fresh_304_is_not_reported_as_a_degradation(self):
        """Only an unanswered origin is stale; a 304 IS the origin answering."""
        url = "https://docs.invalid/a.png"
        with tempfile.TemporaryDirectory() as tmp:
            release = Path(tmp) / "release"
            release.mkdir()
            self._write_lock(release, [self._locked_row(
                self._note("remote-" + "a" * 16, source_path="remote.md", public_url=url), url)])
            doc = self._remote_doc(Path(tmp), url, source_origin="official")
            _n, _f, degradations = self._describe_full(
                release, [doc], self._mock_client(release),
                fetch_remote=self._fake_remote({url: "a" * 64}))
            self.assertEqual([], degradations)

    def test_the_flattened_contact_sheet_is_keyed_on_the_preprocess_version(self):
        """Otherwise bumping the version re-captions against the old sheet."""
        try:
            from PIL import Image
        except ImportError:
            self.skipTest("Pillow is not installed")
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "anim.gif"
            frames = [Image.new("RGB", (4, 4), color) for color in ("red", "blue")]
            frames[0].save(source, save_all=True, append_images=frames[1:], duration=10)
            cache = Path(tmp) / "vl-ready"
            first = pipeline._prepare_vl_image(source, cache)
            self.assertIn(pipeline.VL_PREPROCESS_VERSION, first.name)
            with unittest.mock.patch.object(pipeline, "VL_PREPROCESS_VERSION", "some-other-version"):
                second = pipeline._prepare_vl_image(source, cache)
            self.assertNotEqual(first.name, second.name)

    def test_vl_concurrency_defaults_to_serial(self):
        """Non-answers were a fan-out artifact: 6/40 lost at 8 workers, 0/40 at 1."""
        import inspect

        self.assertEqual(1, inspect.signature(pipeline.describe_assets).parameters["workers"].default)
        build_source = (REPO_ROOT / "scripts/rag_v2/build.py").read_text(encoding="utf-8")
        self.assertIn('"--vl-workers", type=int, default=1', build_source)
        # The HTTP revalidation pass is the opposite case and must stay wired:
        # it is safe to fan out, and it runs on every build.
        self.assertEqual(8, inspect.signature(pipeline.describe_assets).parameters["remote_workers"].default)
        self.assertIn('"--remote-workers", type=int, default=8', build_source)
        self.assertIn("remote_workers=args.remote_workers", build_source)

    def test_only_transient_asset_failures_are_retried(self):
        self.assertTrue(_retryable_asset_failure({"reason": "source_remote_image_unavailable:TimeoutError:x"}))
        self.assertFalse(_retryable_asset_failure({"reason": "source_remote_image_unavailable:HTTPError:HTTP Error 404"}))
        self.assertFalse(_retryable_asset_failure({"reason": "source_remote_image_unavailable:ValueError:remote asset is not an image"}))
        self.assertTrue(_retryable_asset_failure({"reason": "vl_failed:TimeoutError:x"}))
        self.assertTrue(_retryable_asset_failure({"reason": "vl_failed:HTTPError:HTTP Error 400: Bad Request"}))
        self.assertFalse(_retryable_asset_failure({"reason": "missing_local_image"}))
        self.assertFalse(_retryable_asset_failure({"reason": "non_https_image"}))

    def test_github_blob_images_are_downloaded_as_raw_assets(self):
        self.assertEqual(
            "https://raw.githubusercontent.com/org/repo/main/docs/workflow.png",
            _canonical_remote_image_url("https://github.com/org/repo/blob/main/docs/workflow.png?raw=true"),
        )

    def test_decorative_badges_are_filtered_before_vl(self):
        self.assertTrue(_is_decorative_asset("build", "https://img.shields.io/badge/build-passing.svg"))
        self.assertTrue(
            _is_decorative_asset(
                "Open in Colab",
                "https://colab.research.google.com/assets/colab-badge.svg",
            )
        )
        self.assertFalse(_is_decorative_asset("工作流", "https://example.com/workflow.png"))
        self.assertTrue(
            _is_decorative_asset(
                "version",
                "https://camo.githubusercontent.com/x/68747470733a2f2f696d672e736869656c64732e696f2f62616467652f782d79",
            )
        )

    def test_image_magic_recovers_octet_stream_assets(self):
        self.assertEqual("image/png", _image_content_type(b"\x89PNG\r\n\x1a\nrest", "application/octet-stream", "x"))
        self.assertEqual(
            "image/webp",
            _image_content_type(b"RIFF\x00\x00\x00\x00WEBPrest", "application/octet-stream", "x"),
        )
        self.assertIsNone(_image_content_type(b"not an image", "application/octet-stream", "https://x/file"))

    def test_animated_image_is_flattened_for_vl(self):
        try:
            from PIL import Image
        except ImportError:
            self.skipTest("Pillow is optional outside release-build environments")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "animated.gif"
            Image.new("RGB", (4, 4), "red").save(
                source,
                save_all=True,
                append_images=[Image.new("RGB", (4, 4), "blue")],
                duration=100,
                loop=0,
            )
            prepared = _prepare_vl_image(source, root / "cache")
            self.assertNotEqual(source, prepared)
            self.assertEqual(".png", prepared.suffix)
            with Image.open(prepared) as flattened:
                self.assertEqual((4, 8), flattened.size)

    def test_all_supported_image_syntaxes_are_normalized(self):
        text = (
            "![跨行](\nhttps://example.com/a.png)\n"
            "<img src=\"https://example.com/b.webp?x=1\" alt=\"HTML 图\" width=\"10\">\n"
            "[直接图片](https://example.com/c.jpg)\n"
            "![本地空格](<images/a bad result.png>)\n"
            "![](https://img.shields.io/badge/📖 arXiv-red)"
        )
        normalized = normalize_image_markup(text)
        self.assertIn("![跨行](https://example.com/a.png)", normalized)
        self.assertIn("![HTML 图](https://example.com/b.webp?x=1)", normalized)
        self.assertIn("![直接图片](https://example.com/c.jpg)", normalized)
        self.assertIn("![本地空格](images/a%20bad%20result.png)", normalized)
        self.assertIn("![](https://img.shields.io/badge/📖%20arXiv-red)", normalized)


class DigestPinTests(unittest.TestCase):
    """Four SHA256 literals were hand-edited into a Go file every release."""

    def test_pins_are_rewritten_without_touching_the_comments(self):
        source = (
            "// CorpusDigestExpected pins deploy/kb/stage2b_w0.jsonl.\n"
            "//\n"
            "// Every source_path changed in this rebuild.\n"
            'const CorpusDigestExpected = "' + "a" * 64 + '"\n'
            "\n"
            "// Dead, deliberately frozen.\n"
            'const EmbeddingDigestExpected = "' + "b" * 64 + '"\n'
            'const ExternalCorpusDigestExpected = "' + "c" * 64 + '"\n'
        )
        rewritten, changed = release.rewrite_pins(source, {
            "CorpusDigestExpected": "d" * 64,
            "ExternalCorpusDigestExpected": "c" * 64,
        })
        self.assertIn("// Every source_path changed in this rebuild.", rewritten)
        self.assertIn('const CorpusDigestExpected = "' + "d" * 64 + '"', rewritten)
        # The dead legacy pin is not in PINNED_ARTIFACTS and must not move.
        self.assertIn('const EmbeddingDigestExpected = "' + "b" * 64 + '"', rewritten)
        # Reported only when it actually changed.
        self.assertEqual({"CorpusDigestExpected": ("a" * 64, "d" * 64)}, changed)

    def test_a_renamed_constant_is_an_error_not_a_silent_skip(self):
        """Otherwise a rename leaves a stale digest pinned and nothing says so."""
        with self.assertRaises(ValueError):
            release.rewrite_pins("const SomethingElse = \"" + "a" * 64 + "\"\n",
                                 {"CorpusDigestExpected": "d" * 64})

    def test_the_real_digest_file_declares_every_pin_we_rewrite(self):
        source = (REPO_ROOT / "internal/knowledge/corpus_digest.go").read_text(encoding="utf-8")
        rewritten, _changed = release.rewrite_pins(
            source, {name: "e" * 64 for name in release.PINNED_ARTIFACTS})
        self.assertEqual(len(release.PINNED_ARTIFACTS), rewritten.count("e" * 64))
        self.assertNotIn("EmbeddingDigestExpected\"", release.PINNED_ARTIFACTS)

    def test_the_committed_pins_match_the_promoted_artifacts(self):
        """The release command computes these; drift means a bad publish."""
        pins = release.compute_pins(REPO_ROOT / "deploy/kb")
        source = (REPO_ROOT / "internal/knowledge/corpus_digest.go").read_text(encoding="utf-8")
        _rewritten, changed = release.rewrite_pins(source, pins)
        self.assertEqual({}, changed, msg=f"corpus_digest.go is stale: {changed}")


class VendoredSourceTests(unittest.TestCase):
    """The six ZIP inputs used to exist only on one laptop.

    Vendoring them makes a release reproducible from a clean checkout, but
    "the file is present" is a much weaker claim than "the file is the one that
    built the shipped corpus". release_manifest.json already records a sha256
    per source, so the join is free -- and without it a re-export dropped into
    this directory would rebuild a different corpus with nothing saying so.
    """

    SOURCES = REPO_ROOT / "deploy/kb/v2/sources"

    def _zip_sources(self):
        manifest = json.loads(
            (REPO_ROOT / "deploy/kb/v2/release_manifest.json").read_text(encoding="utf-8"))
        return [s for s in manifest["sources"] if s.get("kind") == "zip"]

    def test_every_zip_the_manifest_names_is_vendored_and_matches_its_digest(self):
        declared = self._zip_sources()
        self.assertEqual(6, len(declared), "the build requires exactly 3 FAQ + 3 external ZIPs")
        for source in declared:
            path = self.SOURCES / source["filename"]
            with self.subTest(source=source["id"]):
                self.assertTrue(path.is_file(), f"{source['id']} is not vendored at {path}")
                # The same function build.py:116,126 used to write the manifest
                # entry, so the assertion cannot drift from its producer.
                actual = pipeline.sha256_file(path)
                self.assertEqual(
                    source["sha256"], actual,
                    msg=(f"{source['id']} ({source['filename']}) does not match the release "
                         f"manifest. Building from it would produce a different corpus."))

    def test_no_unexpected_file_sits_in_the_sources_directory(self):
        """A stray ZIP is how the wrong export gets passed positionally."""
        declared = {s["filename"] for s in self._zip_sources()}
        present = {p.name for p in self.SOURCES.iterdir() if p.is_file()}
        self.assertEqual(declared, present)


class ReleaseDiffTests(unittest.TestCase):
    """The publish gate is a human; this is the only thing they get to read."""

    def _chunk(self, doc: str, heading: str, content: str, area: str = "billing_rule"):
        return {
            "chunk_id": "v2-" + area + "-" + pipeline.sha256_bytes(
                (doc + heading + content).encode("utf-8"))[:16],
            "content": content,
            "heading_path": [heading] if heading else [],
            "product_area": area,
            "source_refs": [doc],
            "document_id": "doc-" + pipeline.sha256_bytes(doc.encode("utf-8"))[:16],
        }

    def test_an_edited_section_reads_as_an_edit_not_a_delete_and_an_add(self):
        """chunk_id digests the content, so id-based diffing shows only churn."""
        old = [self._chunk("src:a.md", "计费", "旧的一段话")]
        new = [self._chunk("src:a.md", "计费", "改过的一段话，更长一些")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["sections_added"])
        self.assertEqual([], result["sections_removed"])
        self.assertEqual(1, len(result["sections_changed"]))
        self.assertEqual("计费", result["sections_changed"][0]["heading"])
        self.assertEqual(5, result["sections_changed"][0]["runes_before"])

    def test_a_document_that_only_moved_is_reported_as_moved(self):
        """The pages/ -> content/ migration renamed 227 documents at once."""
        old = [self._chunk("src:pages/x.md", "标题", "一模一样的内容")]
        new = [self._chunk("src:content/x.md", "标题", "一模一样的内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["documents_added"])
        self.assertEqual([], result["documents_removed"])
        self.assertEqual([{"from": "src:pages/x.md", "to": "src:content/x.md",
                           "content_changed": False}], result["documents_moved"])

    def test_a_document_that_moved_and_changed_is_paired_by_path(self):
        """.md -> .mdx rewrote the body too, so content fingerprints miss it."""
        old = [self._chunk("src:pages/gpus/x.md", "标题", "旧内容")]
        new = [self._chunk("src:content/gpus/x.mdx", "标题", "新内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["documents_added"])
        self.assertEqual([], result["documents_removed"])
        self.assertEqual(1, len(result["documents_moved"]))
        self.assertTrue(result["documents_moved"][0]["content_changed"])

    def test_an_ambiguous_rename_is_not_guessed(self):
        """Two candidates for one stem: pairing either would hide a real add."""
        old = [self._chunk("src:pages/a/x.md", "标题", "旧内容")]
        new = [self._chunk("src:content/a/x.md", "标题", "新内容甲"),
               self._chunk("src:other/a/x.md", "标题", "新内容乙")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["documents_moved"])
        self.assertEqual(["src:pages/a/x.md"], result["documents_removed"])
        self.assertEqual(2, len(result["documents_added"]))

    def test_a_move_is_never_paired_across_sources_by_content(self):
        """The external corpus holds three independent third-party snapshots.

        A guide deleted from one and added to another is two real events. Pairing
        them as a move deletes both from the report -- and those are precisely
        the rows the reviewer is there to see.
        """
        old = [self._chunk("external-comfyui:docs/guide.md", "标题", "一模一样的内容")]
        new = [self._chunk("external-voice-audio:docs/guide.md", "标题", "一模一样的内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["documents_moved"])
        self.assertEqual(["external-comfyui:docs/guide.md"], result["documents_removed"])
        self.assertEqual(["external-voice-audio:docs/guide.md"], result["documents_added"])

    def test_a_move_is_never_paired_across_sources_by_path(self):
        """Same trap in the second tier, where the content differs too."""
        old = [self._chunk("external-comfyui:pages/a/guide.md", "标题", "旧内容")]
        new = [self._chunk("external-voice-audio:content/a/guide.mdx", "标题", "新内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual([], result["documents_moved"])
        self.assertEqual(1, len(result["documents_removed"]))
        self.assertEqual(1, len(result["documents_added"]))

    def test_a_move_within_one_source_still_pairs(self):
        """Scoping to the source must not disable the thing pairing is for."""
        old = [self._chunk("external-comfyui:pages/a/guide.md", "标题", "旧内容")]
        new = [self._chunk("external-comfyui:content/a/guide.mdx", "标题", "新内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual(1, len(result["documents_moved"]))
        self.assertTrue(result["documents_moved"][0]["content_changed"])

    def test_a_genuinely_new_document_survives_rename_detection(self):
        old = [self._chunk("src:pages/x.md", "标题", "内容")]
        new = [self._chunk("src:content/x.md", "标题", "内容"),
               self._chunk("src:content/brand-new.md", "新标题", "全新内容")]
        result = release_diff.diff_corpus(old, new)
        self.assertEqual(["src:content/brand-new.md"], result["documents_added"])
        self.assertEqual(1, len(result["documents_moved"]))

    def test_a_caption_rewritten_for_identical_bytes_is_called_out_as_noise(self):
        """Same asset_id means same image; a different caption is the model."""
        old = {"contract": "c1", "notes": [{"asset_id": "remote-aaaa", "description": "旧说明"}]}
        new = {"contract": "c1", "notes": [{"asset_id": "remote-aaaa", "description": "新说明"},
                                           {"asset_id": "remote-bbbb", "description": "新图"}]}
        result = release_diff.diff_captions(old, new)
        self.assertEqual(["remote-bbbb"], result["images_added"])
        self.assertEqual([], result["images_removed"])
        self.assertEqual(1, len(result["captions_rewritten_for_identical_bytes"]))
        markdown = release_diff.render_markdown({
            "headline": {}, "corpora": {}, "captions": result})
        self.assertIn("字节未变但说明变了", markdown)

    def test_a_contract_change_says_the_caption_churn_is_expected(self):
        old = {"contract": "c1", "notes": [{"asset_id": "remote-aaaa", "description": "旧说明"}]}
        new = {"contract": "c2", "notes": [{"asset_id": "remote-aaaa", "description": "新说明"}]}
        markdown = release_diff.render_markdown({
            "headline": {}, "corpora": {}, "captions": release_diff.diff_captions(old, new)})
        self.assertIn("caption 契约变了", markdown)

    def test_build_degradations_reach_the_reviewer(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("old_i.jsonl", "new_i.jsonl", "old_e.jsonl", "new_e.jsonl"):
                (root / name).write_text(json.dumps(self._chunk("src:a.md", "h", "c")) + "\n",
                                         encoding="utf-8")
            (root / "asset_report.json").write_text(json.dumps({
                "failures": [{"severity": "error"}],
                "degradations": [{"source": "a.md", "ref": "https://x/y.png", "severity": "error"}],
            }), encoding="utf-8")
            report = release_diff.build_report(
                old_internal=root / "old_i.jsonl", new_internal=root / "new_i.jsonl",
                old_external=root / "old_e.jsonl", new_external=root / "new_e.jsonl",
                asset_report=root / "asset_report.json")
            self.assertEqual(1, len(report["assets"]["degradations"]))
            self.assertEqual(1, report["assets"]["blocking_failures"])
            markdown = release_diff.render_markdown(report)
            self.assertIn("未能回源校验", markdown)
            self.assertIn("必需图片处理失败", markdown)

    def test_an_asset_report_predating_degradations_is_not_read_as_clean(self):
        """A report with no `degradations` key never examined the question.

        The shipped deploy/kb/v2/asset_report.json is exactly this shape --
        described/failures/published/runtime_mode and nothing else -- because it
        was written before build.py emitted the key. Coercing the absence to []
        renders it as "nothing degraded", which is a clean bill of health from a
        file that never looked.
        """
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("old_i.jsonl", "new_i.jsonl", "old_e.jsonl", "new_e.jsonl"):
                (root / name).write_text(json.dumps(self._chunk("src:a.md", "h", "c")) + "\n",
                                         encoding="utf-8")
            (root / "asset_report.json").write_text(json.dumps({
                "described": 1940, "failures": [], "published": 0, "runtime_mode": "full",
            }), encoding="utf-8")
            report = release_diff.build_report(
                old_internal=root / "old_i.jsonl", new_internal=root / "new_i.jsonl",
                old_external=root / "old_e.jsonl", new_external=root / "new_e.jsonl",
                asset_report=root / "asset_report.json")
            self.assertIsNone(report["assets"]["degradations"])
            self.assertFalse(report["assets"]["degradations_reported"])
            self.assertIn("回源校验情况未知", release_diff.render_markdown(report))

    def test_a_report_that_did_look_and_found_nothing_says_nothing(self):
        """The control: an empty list means examined-and-clean, and stays silent."""
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("old_i.jsonl", "new_i.jsonl", "old_e.jsonl", "new_e.jsonl"):
                (root / name).write_text(json.dumps(self._chunk("src:a.md", "h", "c")) + "\n",
                                         encoding="utf-8")
            (root / "asset_report.json").write_text(json.dumps({
                "described": 3, "failures": [], "degradations": [],
            }), encoding="utf-8")
            report = release_diff.build_report(
                old_internal=root / "old_i.jsonl", new_internal=root / "new_i.jsonl",
                old_external=root / "old_e.jsonl", new_external=root / "new_e.jsonl",
                asset_report=root / "asset_report.json")
            self.assertTrue(report["assets"]["degradations_reported"])
            markdown = release_diff.render_markdown(report)
            self.assertNotIn("回源校验情况未知", markdown)
            self.assertNotIn("需要人看的构建降级", markdown)


class BuildEndToEndTests(unittest.TestCase):
    """The only coverage that drives build.main's real argv.

    Everything else in this file tests pipeline functions directly, which means
    a flag can thread correctly into a helper and still never reach the corpus.
    These fixtures are tiny and --skip-vl/--skip-semantic keep them offline, so
    the whole class is a rounding error on runtime.
    """

    def _zip_of(self, path: Path, files: dict[str, str]) -> None:
        with zipfile.ZipFile(path, "w") as archive:
            for name, body in files.items():
                archive.writestr(name, body)

    def _inputs(self, root: Path) -> list[str]:
        """Three FAQ ZIPs, three external ZIPs, a docs tree and a legacy slice."""
        body = "# 标题\n\n这是一段足够长的正文，用来越过 40 字符的下限，" \
               "这样收集器不会把它当成空文档丢掉。\n"
        docs = root / "docs"
        (docs / "content").mkdir(parents=True)
        (docs / "content" / "guide.md").write_text(body, encoding="utf-8")

        faq_zips = []
        for index in range(3):
            path = root / f"faq{index}.zip"
            self._zip_of(path, {f"faq{index}.md": body})
            faq_zips.append(path)

        external_zips = []
        for index, package in enumerate(("comfyui", "digital-human", "voice-audio")):
            path = root / f"ext{index}.zip"
            source_type = {
                "comfyui": "official_tutorial",
                "digital-human": "runtime_inventory",
                "voice-audio": "official_tutorial",
            }[package]
            manifest = {"files": [{"path": "a.md", "title": "外部文档",
                                   "source_type": source_type}]}
            self._zip_of(path, {"manifest.json": json.dumps(manifest), "a.md": body})
            external_zips.append(path)

        # Field-for-field the shape of a real row in
        # deploy/kb/v2/legacy_external_lock.jsonl, so validate_chunks sees the
        # same contract production does rather than one this test invented.
        legacy = root / "legacy.jsonl"
        legacy.write_text(json.dumps({
            "chunk_id": "ext-legacy-001",
            "kb_version": "kb.external.w0.2026-06-06",
            "source_type": "faq",
            "source_origin": "external_official",
            "product_area": "inference_serving",
            "acl": "customer_safe",
            "title": "遗留外部语料",
            "question_patterns": ["遗留语料怎么保留"],
            "content": "遗留外部语料的一段内容，它的源快照已经不存在，因此只能原样保留。",
            "source_refs": ["legacy-docs:one.md"],
            "asset_refs": [],
            "confidence": "high",
            "valid_from": "2026-06-06",
            "evidence_kind": "knowledge",
            "surface_url": None,
            "retrieval_score_hint": None,
        }, ensure_ascii=False) + "\n", encoding="utf-8")

        argv = ["--internal-docs", str(docs), "--internal-revision", "0" * 40]
        for path in faq_zips:
            argv += ["--faq-zip", str(path)]
        for path in external_zips:
            argv += ["--external-zip", str(path)]
        argv += ["--legacy-external", str(legacy), "--skip-vl", "--skip-semantic"]
        return argv

    def _build(self, root: Path, out: str, *dates: str) -> tuple[list[dict], list[dict]]:
        out_dir = root / out
        argv = [*self._argv, "--out-dir", str(out_dir), *dates]
        self.assertEqual(0, build.main(argv))
        return (
            [json.loads(line) for line in
             (out_dir / "stage2b_v2.jsonl").read_text(encoding="utf-8").splitlines() if line],
            [json.loads(line) for line in
             (out_dir / "external_v2.jsonl").read_text(encoding="utf-8").splitlines() if line],
        )

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self._argv = self._inputs(self.root)
        self.addCleanup(self._tmp.cleanup)

    def test_one_date_still_stamps_both_corpora(self):
        """The default has to be byte-for-byte the old behaviour."""
        internal, external = self._build(self.root, "one", "--valid-from", "2026-08-16")
        self.assertTrue(internal and external)
        self.assertEqual({"2026-08-16"}, {row["valid_from"] for row in internal})
        self.assertEqual({"kb.platform.v2.2026-08-16"}, {row["kb_version"] for row in internal})
        rebuilt = [row for row in external if not row.get("legacy_unrebuildable")]
        self.assertTrue(rebuilt)
        self.assertEqual({"2026-08-16"}, {row["valid_from"] for row in rebuilt})
        self.assertEqual({"kb.external.v2.2026-08-16"}, {row["kb_version"] for row in external})

    def test_the_external_date_can_be_held_back_independently(self):
        internal, external = self._build(
            self.root, "split",
            "--valid-from", "2026-08-16", "--external-valid-from", "2026-07-15")
        self.assertEqual({"2026-08-16"}, {row["valid_from"] for row in internal})
        self.assertEqual({"kb.platform.v2.2026-08-16"}, {row["kb_version"] for row in internal})
        self.assertEqual({"kb.external.v2.2026-07-15"}, {row["kb_version"] for row in external})

    def test_a_docs_only_rebuild_leaves_the_external_corpus_byte_identical(self):
        """The property the flag exists for, asserted on the bytes.

        Without --external-valid-from this is exactly what fails: every external
        row's kb_version moves, so the corpus digest moves, so the ~63 MB sidecar
        is rewritten under a new name and two Go pins follow it -- for a release
        in which no external source changed.
        """
        _, july = self._build(
            self.root, "a", "--valid-from", "2026-07-15", "--external-valid-from", "2026-07-15")
        _, august = self._build(
            self.root, "b", "--valid-from", "2026-08-16", "--external-valid-from", "2026-07-15")
        self.assertEqual(
            (self.root / "a" / "external_v2.jsonl").read_bytes(),
            (self.root / "b" / "external_v2.jsonl").read_bytes(),
        )
        # …and the mutation control: the internal corpus DID move, so this is
        # not two identical builds passing an assertion about nothing.
        self.assertNotEqual(
            (self.root / "a" / "stage2b_v2.jsonl").read_bytes(),
            (self.root / "b" / "stage2b_v2.jsonl").read_bytes(),
        )
        self.assertTrue(july and august)

    def test_without_the_flag_the_same_two_builds_do_diverge(self):
        """The inverse of the test above: proves it is the flag doing the work."""
        self._build(self.root, "c", "--valid-from", "2026-07-15")
        self._build(self.root, "d", "--valid-from", "2026-08-16")
        self.assertNotEqual(
            (self.root / "c" / "external_v2.jsonl").read_bytes(),
            (self.root / "d" / "external_v2.jsonl").read_bytes(),
        )

    def test_the_manifest_records_both_dates(self):
        self._build(self.root, "m", "--valid-from", "2026-08-16",
                    "--external-valid-from", "2026-07-15")
        manifest = json.loads(
            (self.root / "m" / "release_manifest.json").read_text(encoding="utf-8"))
        self.assertEqual("2026-08-16", manifest["report"]["internal"]["valid_from"])
        self.assertEqual("2026-07-15", manifest["report"]["external"]["valid_from"])


class ReleaseOrchestrationTests(unittest.TestCase):
    """release.py's own wiring, which nothing exercised before.

    Two of its properties are load-bearing for the shadow phase: the gate needs
    the RELEASED manifest, and the build overwrites that file in place, so it
    has to be snapshotted before anything runs.
    """

    def test_the_baseline_snapshot_includes_the_manifest_the_gate_needs(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp) / "repo"
            (repo / "deploy/kb/v2").mkdir(parents=True)
            for relative in ("deploy/kb/stage2b_w0.jsonl", "deploy/kb/external_w0.jsonl",
                             "deploy/kb/v2/asset_lock.json", "deploy/kb/v2/release_manifest.json"):
                (repo / relative).write_text("{}", encoding="utf-8")
            kept = release.snapshot_released(repo, Path(tmp) / "snap")
            self.assertEqual({"internal", "external", "lock", "manifest"}, set(kept))
            for path in kept.values():
                self.assertTrue(path.is_file())

    def test_a_missing_released_file_is_absent_rather_than_invented(self):
        """The gate must see 'no baseline', not an empty one that reads as clean."""
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp) / "repo"
            (repo / "deploy/kb").mkdir(parents=True)
            (repo / "deploy/kb/stage2b_w0.jsonl").write_text("{}", encoding="utf-8")
            kept = release.snapshot_released(repo, Path(tmp) / "snap")
            self.assertEqual({"internal"}, set(kept))

    def test_a_mistyped_flag_is_an_error_not_a_silent_default(self):
        """--gate-mod enforce must not quietly run in shadow.

        parse_known_args swallowed unknown flags. That is tolerable while every
        flag is advisory and not once enforce exists, because the entire point
        of enforce is that nobody is watching when it runs.

        This test used to assert only `SystemExit`, and it passed for a reason
        that had nothing to do with the flag: argparse abbreviates by default, so
        `--gate-mod` was resolved to `--gate-mode`, `unknown` stayed empty, the
        guard never ran, and release.main got as far as failing on the missing
        env file -- raising the SystemExit the test was reading as success. It
        now pins the MESSAGE, and --repo keeps a parse-level test out of the real
        deploy/kb tree, where it used to leave an untracked release_base.json.
        """
        with tempfile.TemporaryDirectory() as tmp:
            with contextlib.redirect_stderr(io.StringIO()) as err:
                with self.assertRaises(SystemExit) as caught:
                    release.main(["--repo", tmp, "--env", "x", "--gate-mod", "enforce"])
        self.assertEqual(2, caught.exception.code)
        self.assertIn("--gate-mod", err.getvalue())
        self.assertFalse(
            (Path(tmp) / "deploy/kb/v2/release_base.json").exists(),
            "a rejected invocation must not leave a candidate baseline behind")

    def test_an_abbreviated_safety_flag_cannot_slip_past_the_argv_gate(self):
        """`--skip-v` must not become `--skip-vl`.

        G1.argv asks whether "--skip-vl" is in the recorded build_argv, by exact
        list membership. Under argparse's default abbreviation matching the build
        would accept `--skip-v`, skip captioning, and record the abbreviation --
        which G1.argv does not match, so the gate would report a clean build over
        a corpus missing a third of its internal text.
        """
        for flag in ("--skip-v", "--skip-seman", "--allow-stale"):
            with self.subTest(flag=flag):
                with contextlib.redirect_stderr(io.StringIO()) as err:
                    with self.assertRaises(SystemExit):
                        build.main(["--internal-docs", ".", "--internal-revision", "r",
                                    "--faq-zip", "a.zip", "--external-zip", "b.zip",
                                    "--legacy-external", "c.jsonl", "--out-dir", ".",
                                    "--valid-from", "2026-08-16", flag])
                self.assertIn("unrecognized arguments", err.getvalue())
                self.assertIn(flag, err.getvalue())


class EmbeddingRefreshReportTests(unittest.TestCase):
    """The reuse counts as an ARTIFACT, not as a line of job log.

    refresh_embeddings printed reused=/embedded= and nothing consumed it, so a
    build that re-embedded every chunk and one that reused every vector were
    indistinguishable in every file a reviewer or a gate can read. These tests
    drive the real script; the no-change path needs no network, because when
    nothing changed there is nothing to embed.
    """

    def _corpus(self, path, rows):
        path.write_text(
            "".join(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n" for row in rows),
            encoding="utf-8")

    def _row(self, chunk_id, content, title=None):
        return {
            "chunk_id": chunk_id,
            "kb_version": "kb.platform.v2.2026-08-16",
            "valid_from": "2026-08-16",
            "source_type": "runbook",
            "source_origin": "official",
            "product_area": "gpu",
            "acl": "customer_safe",
            "confidence": "high",
            "title": title or ("标题 " + chunk_id),
            "question_patterns": [],
            "content": content,
        }

    def _run_refresh(self, rows_before, rows_after):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        old_corpus = root / "old.jsonl"
        new_corpus = root / "new.jsonl"
        self._corpus(old_corpus, rows_before)
        self._corpus(new_corpus, rows_after)

        digest = compute_lf_sha256(old_corpus)
        sidecar = root / sidecar_filename(digest, "qwen3-embedding-8b")
        write_sidecar(
            sidecar,
            corpus_digest=digest,
            embed_model="qwen3-embedding-8b",
            dim=2,
            rows=[(str(row["chunk_id"]), [0.1, 0.2]) for row in rows_before],
        )

        env_file = root / "env"
        env_file.write_text("MODELVERSE_API_KEY=unused\n", encoding="utf-8")
        report = root / "embedding_refresh_stage2b.json"
        code = refresh_embeddings.main([
            "--old-corpus", str(old_corpus),
            "--new-corpus", str(new_corpus),
            "--old-sidecar", str(sidecar),
            "--out-dir", str(root),
            "--env", str(env_file),
            "--corpus-name", "stage2b",
            "--report", str(report),
        ])
        self.assertEqual(0, code)
        return json.loads(report.read_text(encoding="utf-8"))

    def test_an_unchanged_corpus_reports_full_reuse_and_makes_no_model_call(self):
        rows = [self._row("v2-gpu-a", "第一段正文"), self._row("v2-gpu-b", "第二段正文")]
        record = self._run_refresh(rows, rows)
        self.assertEqual(2, record["reused"])
        self.assertEqual(0, record["embedded"])
        self.assertEqual(2, record["chunks"])
        self.assertEqual(1.0, record["reuse_ratio"])
        self.assertEqual("stage2b", record["corpus"])

    def test_a_rewritten_title_alone_invalidates_the_vector(self):
        """The finding G8 exists for, reproduced against the real script.

        chunk_id hashes source path, position and CONTENT. chunk_repr also
        carries the title and the question patterns. So this row keeps its
        id and loses its vector -- which is how a question-pattern or
        product-area rule change re-embeds an entire corpus while every id
        looks stable and nothing that reads ids notices.

        embed_batch is patched rather than reached: the point is which rows
        the script classifies as changed, and a test that needed the network
        to answer that would be slow, flaky, and would call a paid endpoint
        with a fake key.
        """
        before = [self._row("v2-gpu-a", "同一段正文", title="原标题")]
        after = [self._row("v2-gpu-a", "同一段正文", title="改写过的标题")]
        self.assertEqual(before[0]["chunk_id"], after[0]["chunk_id"],
                         "fixture must keep the id identical, or it tests nothing")
        self.assertEqual(before[0]["content"], after[0]["content"],
                         "fixture must keep the body identical, or it tests content change")

        calls = []

        def fake_embed_batch(texts, **kwargs):
            calls.append(list(texts))
            return [[0.9, 0.9] for _ in texts], 2

        with unittest.mock.patch.object(
                refresh_embeddings, "embed_batch", fake_embed_batch):
            record = self._run_refresh(before, after)

        self.assertEqual(1, record["embedded"],
                         "a title-only edit must count as changed")
        self.assertEqual(0, record["reused"])
        self.assertEqual(0.0, record["reuse_ratio"])
        self.assertEqual(1, len(calls))
        self.assertIn("改写过的标题", calls[0][0])

class ReleaseInputTests(unittest.TestCase):
    """The values the unattended job reads off its own artifacts.

    These decide what a release is BUILT FROM, so a wrong answer here is not a
    crash, it is a corpus rebuilt against the wrong docs revision or restamped
    for nothing. Each one used to be a `sed` or an embedded `python -c`, where
    none of this could be asserted.
    """

    def test_the_pinned_revision_comes_from_the_shipped_manifest(self):
        revision = release_inputs.pinned_docs_revision(
            REPO_ROOT / "deploy/kb/v2/release_manifest.json")
        self.assertRegex(revision, r"^[0-9a-f]{40}$")

    def test_a_manifest_without_a_docs_source_is_an_error_not_an_empty_string(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "m.json"
            path.write_text(json.dumps({"sources": [{"id": "faq-usage", "kind": "zip"}]}),
                            encoding="utf-8")
            with self.assertRaises(SystemExit):
                release_inputs.pinned_docs_revision(path)

    def test_an_empty_revision_is_an_error_too(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "m.json"
            path.write_text(json.dumps(
                {"sources": [{"id": "gitlab-compshare-docs", "revision": ""}]}), encoding="utf-8")
            with self.assertRaises(SystemExit):
                release_inputs.pinned_docs_revision(path)

    def test_the_external_stamp_comes_from_kb_version_on_the_real_corpus(self):
        self.assertEqual(
            release_inputs.corpus_valid_from(REPO_ROOT / "deploy/kb/external_w0.jsonl"),
            "2026-08-14")

    def test_the_stamp_is_not_taken_from_valid_from(self):
        """The shipped external corpus genuinely mixes two valid_from values.

        A max() over valid_from returns the same answer today, so this fixture
        makes the two disagree: the legacy row is stamped LATER than the
        rebuilt one, which is the case that separates the two readings.
        """
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "external.jsonl"
            path.write_text("".join(json.dumps(row) + "\n" for row in [
                {"chunk_id": "a", "kb_version": "kb.external.v2.2026-07-15",
                 "valid_from": "2026-07-15"},
                {"chunk_id": "b", "kb_version": "kb.external.v2.2026-07-15",
                 "valid_from": "2026-12-31"},
            ]), encoding="utf-8")
            self.assertEqual("2026-07-15", release_inputs.corpus_valid_from(path))

    def test_a_corpus_with_two_kb_versions_refuses_to_guess(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "external.jsonl"
            path.write_text("".join(json.dumps(row) + "\n" for row in [
                {"chunk_id": "a", "kb_version": "kb.external.v2.2026-07-15"},
                {"chunk_id": "b", "kb_version": "kb.external.v2.2026-08-16"},
            ]), encoding="utf-8")
            with self.assertRaises(SystemExit):
                release_inputs.corpus_valid_from(path)

    def test_a_kb_version_with_no_date_stamp_refuses_to_guess(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "external.jsonl"
            path.write_text(json.dumps({"chunk_id": "a", "kb_version": "kb.external.w0"}) + "\n",
                            encoding="utf-8")
            with self.assertRaises(SystemExit):
                release_inputs.corpus_valid_from(path)

    def test_release_inputs_needs_nothing_outside_the_standard_library(self):
        """knowledge-tick runs it on `apk add python3` and nothing else.

        An import of pipeline here would drag Pillow onto the cheap job whose
        whole purpose is to answer one question without paying for the build.
        """
        source = (REPO_ROOT / "scripts/rag_v2/release_inputs.py").read_text(encoding="utf-8")
        for forbidden in ("pipeline", "PIL", "requests", "fitz", "pypdf"):
            self.assertNotIn(f"import {forbidden}", source)
            self.assertNotIn(f"from {forbidden}", source)


class ReleaseGateTests(unittest.TestCase):
    """One passing candidate, then one mutation per assertion.

    Given this tree's history of gates that could not fail -- the discarded
    release_diff return value, `degradations` reading absent as clean, --skip-vl
    making the caption diff compare a file to itself -- an assertion that has
    never been observed red is not evidence of anything. So every check below
    has a fixture that turns it red, and the baseline proves the fixture is
    otherwise clean.
    """

    OLD, NEW = "2026-07-15", "2026-08-16"
    OLD_REV, NEW_REV = "a" * 40, "b" * 40

    def _chunk(self, doc: str, content: str, *, kb: str, valid: str, index: int = 0):
        source, _, path = doc.partition(":")
        return {
            "chunk_id": f"v2-{pipeline.sha256_bytes((doc + content).encode())[:16]}",
            "kb_version": kb,
            "valid_from": valid,
            "source_type": "platform_public_doc",
            "source_origin": "official",
            "product_area": "gpu",
            "acl": "customer_safe",
            "title": path or source,
            "question_patterns": [],
            "content": content,
            "heading_path": [f"h{index}"],
            "source_refs": [doc],
            "asset_refs": [],
            "confidence": "high",
            "evidence_kind": "knowledge",
        }

    def _jsonl(self, path: Path, rows):
        path.write_text(
            "".join(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n" for row in rows),
            encoding="utf-8")

    def _manifest(self, revision: str, zips: dict[str, str], argv: list[str],
                  embeddings: dict | None = None):
        sources = [{"id": "gitlab-compshare-docs", "kind": "git", "revision": revision}]
        sources += [{"id": name, "kind": "zip", "filename": f"{name}.zip", "sha256": digest}
                    for name, digest in sorted(zips.items())]
        if embeddings is None:
            # An ordinary docs-only release: one internal chunk re-embedded,
            # the frozen external corpus reusing everything.
            embeddings = {
                "stage2b": {"corpus": "stage2b", "chunks": 3, "reused": 2, "embedded": 1,
                            "reuse_ratio": 0.6667, "embed_model": "qwen3-embedding-8b"},
                "external": {"corpus": "external", "chunks": 1, "reused": 1, "embedded": 0,
                             "reuse_ratio": 1.0, "embed_model": "qwen3-embedding-8b"},
            }
        return {"schema_version": "compshare.rag.release.v2", "sources": sources,
                "models": {"vl": "Qwen/Qwen3-VL-235B-A22B-Instruct"},
                "report": {"build_argv": argv, "embeddings": embeddings}}

    def _lock(self, captions: dict[str, str], owner: str = "gitlab-compshare-docs:content/a.md"):
        source, _, path = owner.partition(":")
        return {
            "schema_version": 1,
            "contract": "c" * 64,
            "failures": [],
            "notes": [{"asset_id": asset, "description": text, "source_id": source,
                       "source_path": path, "include_in_rag": True,
                       "model": "Qwen/Qwen3-VL-235B-A22B-Instruct"}
                      for asset, text in sorted(captions.items())],
        }

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        root = Path(self._tmp.name)
        self.released = root / "released"
        self.candidate = root / "candidate"
        self.released.mkdir()
        self.candidate.mkdir()

        zips = {f"zip{i}": pipeline.sha256_bytes(f"z{i}".encode()) for i in range(6)}
        old_kb, new_kb = f"kb.platform.v2.{self.OLD}", f"kb.platform.v2.{self.NEW}"

        self.old_internal = [
            self._chunk("gitlab-compshare-docs:content/a.md", "原来的一段平台文档正文",
                        kb=old_kb, valid=self.OLD),
            self._chunk("gitlab-compshare-docs:content/b.md", "另一篇没有改动的文档",
                        kb=old_kb, valid=self.OLD),
            self._chunk("faq-usage:usage.md", "售后群里整理的一条常见问题",
                        kb=old_kb, valid=self.OLD),
        ]
        # The FAQ row moves its kb_version and valid_from like every other
        # internal row -- the internal corpus carries ONE kb_version. The
        # baseline passing G4.faq is what proves the projection excludes them.
        self.new_internal = [
            self._chunk("gitlab-compshare-docs:content/a.md", "改写过的一段平台文档正文，更长了一点",
                        kb=new_kb, valid=self.NEW),
            self._chunk("gitlab-compshare-docs:content/b.md", "另一篇没有改动的文档",
                        kb=new_kb, valid=self.NEW),
            self._chunk("faq-usage:usage.md", "售后群里整理的一条常见问题",
                        kb=new_kb, valid=self.NEW),
        ]
        self.external = [self._chunk("external-comfyui:guide.md", "外部快照里的一段内容",
                                     kb=f"kb.external.v2.{self.OLD}", valid=self.OLD)]

        self.old_manifest = self._manifest(self.OLD_REV, zips, [])
        self.new_manifest = self._manifest(
            self.NEW_REV, zips,
            ["--internal-revision", self.NEW_REV, "--valid-from", self.NEW,
             "--external-valid-from", self.OLD])
        self.report = {"described": 1908, "published": 0, "runtime_mode": "caption_only",
                       "failures": [], "degradations": []}
        self.old_lock = self._lock({"remote-1": "一张截图"})
        self.new_lock = self._lock({"remote-1": "一张截图"})
        self.docs_diff = "content/a.md\n"
        self._write()

    def _write(self):
        self._jsonl(self.released / "stage2b_w0.jsonl", self.old_internal)
        self._jsonl(self.released / "external_w0.jsonl", self.external)
        self._jsonl(self.candidate / "stage2b_v2.jsonl", self.new_internal)
        self._jsonl(self.candidate / "external_v2.jsonl", self.external)
        (self.released / "release_manifest.json").write_text(
            json.dumps(self.old_manifest, ensure_ascii=False), encoding="utf-8")
        (self.candidate / "release_manifest.json").write_text(
            json.dumps(self.new_manifest, ensure_ascii=False), encoding="utf-8")
        (self.candidate / "asset_report.json").write_text(
            json.dumps(self.report, ensure_ascii=False), encoding="utf-8")
        (self.released / "asset_lock.json").write_text(
            json.dumps(self.old_lock, ensure_ascii=False), encoding="utf-8")
        (self.candidate / "asset_lock.json").write_text(
            json.dumps(self.new_lock, ensure_ascii=False), encoding="utf-8")
        (self.candidate / "docs_diff.txt").write_text(self.docs_diff, encoding="utf-8")
        # Produced by its real producer rather than hand-shaped, so this fixture
        # cannot drift from what release.py actually writes.
        report = release_diff.build_report(
            old_internal=self.released / "stage2b_w0.jsonl",
            new_internal=self.candidate / "stage2b_v2.jsonl",
            old_external=self.released / "external_w0.jsonl",
            new_external=self.candidate / "external_v2.jsonl",
            old_lock=self.released / "asset_lock.json",
            new_lock=self.candidate / "asset_lock.json",
            asset_report=self.candidate / "asset_report.json")
        (self.candidate / "release_diff.json").write_text(
            json.dumps(report, ensure_ascii=False, sort_keys=True), encoding="utf-8")

    def _run(self, **overrides):
        inputs = release_gate.Inputs(
            release_dir=self.candidate,
            released_internal=self.released / "stage2b_w0.jsonl",
            released_external=self.released / "external_w0.jsonl",
            released_manifest=self.released / "release_manifest.json",
            docs_diff=self.candidate / "docs_diff.txt",
            today=date.fromisoformat(self.NEW),
        )
        return release_gate.verdict(release_gate.evaluate(inputs, **overrides))

    def _check(self, result, check_id):
        for finding in result["findings"]:
            if finding["id"] == check_id:
                return finding
        self.fail(f"no check named {check_id}; got {[f['id'] for f in result['findings']]}")

    # ---- the baseline ----------------------------------------------------
    def test_an_ordinary_docs_only_release_is_auto_publishable(self):
        result = self._run()
        failed = [f for f in result["findings"] if not f["ok"]]
        self.assertEqual([], failed, msg=f"baseline should be clean, got {failed}")
        self.assertTrue(result["auto_publishable"])
        self.assertEqual([], result["evidence_missing"])

    # ---- one mutation per assertion --------------------------------------
    def test_G0_fails_when_the_docs_revision_did_not_move(self):
        self.new_manifest = self._manifest(self.OLD_REV, {}, [])
        self.new_manifest["sources"] += self.old_manifest["sources"][1:]
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G0")["ok"])
        self.assertFalse(result["auto_publishable"])

    def test_G1_argv_fails_when_a_safety_flag_was_passed(self):
        self.new_manifest["report"]["build_argv"].append("--skip-vl")
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G1.argv")["ok"])
        self.assertIn("--skip-vl", self._check(result, "G1.argv")["detail"])

    def test_G1_captions_fails_when_nothing_was_described(self):
        """--skip-vl also removes its own trace from argv if someone edits it."""
        self.report["described"] = 0
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G1.captions")["ok"])

    def test_G1_stale_remote_reports_evidence_missing_not_clean(self):
        self.report.pop("degradations")
        self._write()
        result = self._run()
        finding = self._check(result, "G1.stale-remote")
        self.assertFalse(finding["ok"])
        self.assertTrue(finding["evidence_missing"])
        self.assertIn("G1.stale-remote", result["evidence_missing"])

    def test_G1_stale_remote_fails_on_an_unrevalidated_platform_image(self):
        self.report["degradations"] = [{"ref": "x.png", "severity": "error"}]
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G1.stale-remote")["ok"])

    def test_G1_lock_fails_when_the_caption_lock_was_never_written(self):
        self.new_lock = {"contract": "", "notes": [], "failures": [], "schema_version": 1}
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G1.lock")["ok"])

    def test_G2_fails_when_an_input_zip_was_swapped(self):
        for source in self.new_manifest["sources"]:
            if source.get("kind") == "zip":
                source["sha256"] = pipeline.sha256_bytes(b"a different export")
                break
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G2")["ok"])

    def test_G3_fails_on_a_chunk_that_is_not_in_effect_yet(self):
        self.new_internal[0]["valid_from"] = "2099-01-01"
        self._write()
        result = self._run()
        finding = self._check(result, "G3")
        self.assertFalse(finding["ok"])
        self.assertIn("2099-01-01", finding["detail"])

    def test_G3_fails_on_an_unparsable_date(self):
        self.new_internal[0]["valid_from"] = "not-a-date"
        self._write()
        self.assertFalse(self._check(self._run(), "G3")["ok"])

    def test_G4_faq_fails_when_an_unreviewed_faq_chunk_moves(self):
        self.new_internal[2]["content"] = "有人悄悄改了售后 FAQ 的正文"
        self._write()
        result = self._run()
        self.assertFalse(self._check(result, "G4.faq")["ok"])
        self.assertFalse(result["auto_publishable"])

    def test_G4_external_names_the_stamp_when_only_the_stamp_moved(self):
        restamped = [dict(row, kb_version=f"kb.external.v2.{self.NEW}", valid_from=self.NEW)
                     for row in self.external]
        self._write()
        self._jsonl(self.candidate / "external_v2.jsonl", restamped)
        result = self._run()
        finding = self._check(result, "G4.external")
        self.assertFalse(finding["ok"])
        self.assertIn("--external-valid-from", finding["detail"])

    def test_G4_external_names_content_when_the_content_moved(self):
        self._write()
        self._jsonl(self.candidate / "external_v2.jsonl",
                    [dict(row, content="外部内容被改掉了") for row in self.external])
        finding = self._check(self._run(), "G4.external")
        self.assertFalse(finding["ok"])
        self.assertIn("CONTENT", finding["detail"])

    def test_G5_fails_when_a_changed_document_is_in_no_docs_commit(self):
        self.new_internal[1]["content"] = "这篇文档也变了，但没有对应的 docs 提交"
        self._write()
        result = self._run()
        finding = self._check(result, "G5")
        self.assertFalse(finding["ok"])
        self.assertIn("content/b.md", finding["detail"])

    def test_G5_fails_when_only_served_metadata_changed_on_an_unattributed_document(self):
        # content/b.md is not in docs_diff.txt. Its BODY is byte-identical on
        # both sides, so content_digest matches, the slot key matches, and before
        # sections_metadata_changed existed this produced no entry in any view of
        # the diff -- G5 never saw the document and certified it as attributed.
        # title and question_patterns are what retrieval matches on.
        self.new_internal[1]["title"] = "改写过的标题"
        self.new_internal[1]["question_patterns"] = ["这台机器怎么开机"]
        self._write()

        self.assertEqual(
            self.old_internal[1]["content"], self.new_internal[1]["content"],
            "fixture must keep the body byte-identical, or this tests sections_changed instead")

        result = self._run()
        finding = self._check(result, "G5")
        self.assertFalse(finding["ok"], msg=f"G5 stayed green on a metadata-only edit: {finding}")
        self.assertIn("content/b.md", finding["detail"])
        self.assertIn("title", finding["detail"])
        self.assertFalse(result["auto_publishable"])

    def test_G5_stays_green_when_metadata_changed_on_a_document_the_docs_diff_names(self):
        # The mirror of the test above: the same edit on content/a.md, which the
        # docs diff DOES name, must not start failing releases. A check that
        # cannot pass is removed rather than obeyed.
        self.new_internal[0]["title"] = "改写过的标题"
        self._write()
        result = self._run()
        self.assertTrue(self._check(result, "G5")["ok"])
        self.assertTrue(result["auto_publishable"])

    def test_release_diff_reports_a_metadata_only_edit_to_the_reviewer(self):
        # G5 reading a view the human-facing report does not render would be
        # attribution without review.
        self.new_internal[1]["product_area"] = "billing_rule"
        self._write()
        report = json.loads((self.candidate / "release_diff.json").read_text(encoding="utf-8"))
        internal = next(corpus for name, corpus in report["corpora"].items() if "stage2b" in name)
        rows = internal["sections_metadata_changed"]
        self.assertEqual(1, len(rows), msg=f"expected exactly one metadata-only row, got {rows}")
        self.assertEqual("gitlab-compshare-docs:content/b.md", rows[0]["document"])
        self.assertEqual(["product_area"], rows[0]["fields"])
        self.assertFalse(rows[0]["content_also_changed"])
        markdown = release_diff.render_markdown(report)
        self.assertIn("正文未变、检索字段改动的小节", markdown)
        self.assertIn("product_area", markdown)

    def test_release_stamps_alone_never_count_as_a_metadata_change(self):
        # kb_version and valid_from move on EVERY internal row of every rebuild.
        # If they were in the projection, this baseline would report all three
        # chunks as metadata-changed and G5 would demand a docs commit for the
        # FAQ rows on every single release.
        report = json.loads((self.candidate / "release_diff.json").read_text(encoding="utf-8"))
        internal = next(corpus for name, corpus in report["corpora"].items() if "stage2b" in name)
        self.assertEqual([], internal["sections_metadata_changed"])

    def test_G5_fails_when_the_docs_diff_is_absent_rather_than_passing(self):
        inputs = release_gate.Inputs(
            release_dir=self.candidate,
            released_internal=self.released / "stage2b_w0.jsonl",
            released_external=self.released / "external_w0.jsonl",
            released_manifest=self.released / "release_manifest.json",
            docs_diff=None, today=date.fromisoformat(self.NEW))
        result = release_gate.verdict(release_gate.evaluate(inputs))
        finding = self._check(result, "G5")
        self.assertFalse(finding["ok"])
        self.assertTrue(finding["evidence_missing"])

    def test_G6_fails_when_a_caption_drifts_on_an_untouched_document(self):
        self.old_lock = self._lock({"remote-1": "原来的说明"},
                                   owner="gitlab-compshare-docs:content/b.md")
        self.new_lock = self._lock({"remote-1": "模型这次说了别的"},
                                   owner="gitlab-compshare-docs:content/b.md")
        self._write()
        result = self._run()
        finding = self._check(result, "G6")
        self.assertFalse(finding["ok"])
        self.assertIn("content/b.md", finding["detail"])

    def test_G6_allows_a_caption_rewrite_inside_a_document_that_did_change(self):
        """The control: same drift, but on the document the docs commit touched."""
        self.old_lock = self._lock({"remote-1": "原来的说明"})
        self.new_lock = self._lock({"remote-1": "模型这次说了别的"})
        self._write()
        self.assertTrue(self._check(self._run(), "G6")["ok"])

    def test_G6_fails_when_the_caption_contract_itself_changed(self):
        self.new_lock["contract"] = "d" * 64
        self._write()
        finding = self._check(self._run(), "G6")
        self.assertFalse(finding["ok"])
        self.assertIn("contract", finding["detail"])

    def test_G7_only_records_until_a_threshold_is_set(self):
        self.new_internal = [self.new_internal[2]]
        self._write()
        result = self._run()
        finding = self._check(result, "G7")
        self.assertTrue(finding["ok"])
        self.assertFalse(finding["blocking"])

    def test_G7_blocks_once_a_threshold_is_set(self):
        self.new_internal = [self.new_internal[2]]
        self._write()
        finding = self._check(self._run(max_shrink=0.05), "G7")
        self.assertFalse(finding["ok"])
        self.assertTrue(finding["blocking"])

    def _zips(self):
        return {f"zip{i}": pipeline.sha256_bytes(f"z{i}".encode()) for i in range(6)}

    def _full_re_embed_manifest(self):
        return self._manifest(
            self.NEW_REV, self._zips(), self.new_manifest["report"]["build_argv"],
            embeddings={
                "stage2b": {"corpus": "stage2b", "chunks": 526, "reused": 0,
                            "embedded": 526, "reuse_ratio": 0.0,
                            "embed_model": "qwen3-embedding-8b"},
            })

    def test_G8_records_reuse_without_blocking_until_a_threshold_is_set(self):
        finding = self._check(self._run(), "G8")
        self.assertTrue(finding["ok"])
        self.assertFalse(finding["blocking"])
        self.assertIn("reused 2/3", finding["detail"])
        self.assertIn("external reused 1/1", finding["detail"])

    def test_G8_names_a_silent_full_re_embed(self):
        """0% reuse is the shape that costs a full rebuild and raises nothing.

        The reuse key is chunk_repr -- title + question patterns + content --
        while chunk_id hashes source path, position and content. A rewritten
        question-pattern rule therefore leaves every id in place and
        invalidates every vector: the corpus is correct, the digests bind,
        every other check passes, and the build quietly makes one model call
        per chunk. Nothing but this number can tell the two apart.
        """
        self.new_manifest = self._full_re_embed_manifest()
        self._write()
        finding = self._check(self._run(), "G8")
        self.assertIn("reused 0/526", finding["detail"])
        self.assertIn("NOTHING was reused", finding["detail"])
        # Still non-blocking without a threshold: it reports, it does not judge.
        self.assertTrue(finding["ok"])
        self.assertFalse(finding["blocking"])

    def test_G8_blocks_once_a_threshold_is_set(self):
        self.new_manifest = self._full_re_embed_manifest()
        self._write()
        result = self._run(min_reuse=0.5)
        finding = self._check(result, "G8")
        self.assertFalse(finding["ok"])
        self.assertTrue(finding["blocking"])
        self.assertFalse(result["auto_publishable"])

    def test_G8_treats_an_absent_record_as_missing_evidence_not_as_full_reuse(self):
        """A release built before the numbers were recorded proves nothing.

        This is the failure mode the whole gate was written against: an
        absent key read through `.get(...) or {}` and reported as a clean
        result. No reuse record must never be indistinguishable from a build
        that reused every vector.
        """
        manifest = self._manifest(
            self.NEW_REV, self._zips(), self.new_manifest["report"]["build_argv"])
        del manifest["report"]["embeddings"]
        self.new_manifest = manifest
        self._write()
        finding = self._check(self._run(), "G8")
        self.assertFalse(finding["ok"])
        self.assertTrue(finding["evidence_missing"])

    # ---- the mode contract ------------------------------------------------
    def test_shadow_records_a_rejection_without_failing_the_pipeline(self):
        self.new_internal[2]["content"] = "改了没人评审的 FAQ"
        self._write()
        common = [
            "--release-dir", str(self.candidate),
            "--released-internal", str(self.released / "stage2b_w0.jsonl"),
            "--released-external", str(self.released / "external_w0.jsonl"),
            "--released-manifest", str(self.released / "release_manifest.json"),
            "--docs-diff", str(self.candidate / "docs_diff.txt"),
            "--today", self.NEW,
            "--out-json", str(self.candidate / "gate.json"),
        ]
        self.assertEqual(0, release_gate.main([*common, "--mode", "shadow"]))
        recorded = json.loads((self.candidate / "gate.json").read_text(encoding="utf-8"))
        self.assertFalse(recorded["auto_publishable"])
        self.assertIn("G4.faq", recorded["blocking_failures"])
        self.assertEqual(1, release_gate.main([*common, "--mode", "enforce"]))

    def test_enforce_passes_a_clean_candidate(self):
        self.assertEqual(0, release_gate.main([
            "--release-dir", str(self.candidate),
            "--released-internal", str(self.released / "stage2b_w0.jsonl"),
            "--released-external", str(self.released / "external_w0.jsonl"),
            "--released-manifest", str(self.released / "release_manifest.json"),
            "--docs-diff", str(self.candidate / "docs_diff.txt"),
            "--today", self.NEW, "--mode", "enforce"]))

    def test_every_check_the_gate_emits_has_a_test_that_turns_it_red(self):
        """The meta-assertion: a check nobody proved can fail is not a gate.

        Without this, adding a check whose fixture nobody wrote is invisible --
        it ships green forever and reads as coverage.
        """
        emitted = {f["id"] for f in self._run()["findings"]}
        proven = set()
        for name in dir(self):
            if not name.startswith("test_G"):
                continue
            # test_G1_stale_remote_... -> G1.stale-remote / G1.captions / G0 / G7
            head = name[len("test_"):].split("_fails")[0].split("_only")[0]
            head = head.split("_allows")[0].split("_blocks")[0].split("_names")[0]
            proven.add(head.replace("_", ".", 1).replace("_", "-"))
        self.assertEqual(
            emitted, emitted & proven,
            msg=f"checks with no red fixture: {sorted(emitted - proven)}")


if __name__ == "__main__":
    unittest.main()
