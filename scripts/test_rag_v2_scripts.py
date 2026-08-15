import json
from pathlib import Path
import tempfile
import unittest
import unittest.mock

from scripts.rag_w0.build_corpus_embeddings import write_sidecar

from scripts.rag_v2 import pipeline
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

    def _caption_lock_fixture(self, tmp: Path, doc_path: str):
        """A document with one local image, plus the release dir describe_assets uses."""
        root = tmp / "docs"
        (root / Path(doc_path).parent).mkdir(parents=True, exist_ok=True)
        image = root / "screen.png"
        image.write_bytes(b"\x89PNG\r\n\x1a\n" + b"pretend-png-bytes")
        page = root / doc_path
        page.write_text("说明\n\n![控制台](/screen.png)\n", encoding="utf-8")
        doc = SourceDocument(
            source_id="gitlab-compshare-docs", source_path=doc_path,
            source_kind="public_faq_export", source_origin="official", title="页面",
            text=page.read_text(encoding="utf-8"), surface_url=None, root=root, absolute_path=page,
        )
        return doc, image

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
            note = AssetNote(
                asset_id="asset-" + pipeline.sha256_file(
                    self._caption_lock_fixture(Path(tmp), "pages/guide.md")[1])[:16],
                source_path="pages/guide.md", repo_path=None, public_url="",
                description="控制台创建实例页面", visible_text=["创建实例"], controls=["确认"],
                relations=[], confidence=0.98, model="vl-model", visual_type="ui",
            )
            (release / "asset_lock.json").write_text(json.dumps({
                "schema_version": "compshare.rag.asset-lock.v1",
                "fingerprint": "fingerprint-of-the-previous-corpus",
                "notes": [{"source_id": "gitlab-compshare-docs", "ref": "/screen.png",
                           **{k: v for k, v in vars(note).items()}}],
                "failures": [],
            }, ensure_ascii=False), encoding="utf-8")

            # Same image bytes, new path: exactly what the pages/ -> content/
            # App Router migration did to every document in the corpus.
            moved, _image = self._caption_lock_fixture(Path(tmp), "content/guide.md")

            def explode(*args, **kwargs):
                raise AssertionError("describe_assets called the VL model for an unchanged image")

            client = unittest.mock.Mock(spec=ModelVerseClient)
            client.cache_dir = release / ".cache" / "modelverse"
            client.json_chat.side_effect = explode
            client.cached_json.side_effect = explode

            notes, failures = pipeline.describe_assets(
                [moved], client=client, model="vl-model", fallback_model=None,
                assets_dir=release / "assets", raw_asset_base_url="https://example.invalid/a",
            )

            self.assertEqual([], failures)
            reused = notes[("gitlab-compshare-docs", "content/guide.md", "/screen.png")]
            self.assertEqual("控制台创建实例页面", reused.description)
            self.assertEqual(["创建实例"], reused.visible_text)
            # The note follows the image to its new home rather than keeping the
            # path it was first captioned under.
            self.assertEqual("content/guide.md", reused.source_path)

    def test_reuse_is_refused_when_the_note_came_from_a_different_model(self):
        """Content identity says the bytes match, not that the caption is ours."""
        self.assertEqual(
            "asset-0123456789abcdef",
            pipeline.reuse_key_for_identity("file:0123456789abcdef" + "f" * 48),
        )
        self.assertEqual("url:https://x.invalid/a.png",
                         pipeline.reuse_key_for_identity("url:https://x.invalid/a.png"))
        self.assertIsNone(pipeline.reuse_key_for_identity("missing:src:doc.md:a.png"))

        local = AssetNote(asset_id="asset-0123456789abcdef", source_path="a.md", repo_path=None,
                          public_url="", description="d", visible_text=[], controls=[],
                          relations=[], confidence=0.9, model="m")
        self.assertEqual("asset-0123456789abcdef", pipeline.locked_note_identity(local, "a.png"))
        remote = AssetNote(asset_id="remote-0123456789abcdef", source_path="a.md", repo_path=None,
                           public_url="https://x.invalid/a.png", description="d", visible_text=[],
                           controls=[], relations=[], confidence=0.9, model="m")
        self.assertEqual("url:https://x.invalid/a.png", pipeline.locked_note_identity(remote, "ignored"))
        # A note carrying no content digest must not be matched by identity at
        # all, or every such note would collide onto one key.
        opaque = AssetNote(asset_id="legacy", source_path="a.md", repo_path=None, public_url="",
                           description="d", visible_text=[], controls=[], relations=[],
                           confidence=0.9, model="m")
        self.assertIsNone(pipeline.locked_note_identity(opaque, "a.png"))

    def test_vl_concurrency_defaults_to_serial(self):
        """Non-answers were a fan-out artifact: 6/40 lost at 8 workers, 0/40 at 1."""
        import inspect

        self.assertEqual(1, inspect.signature(pipeline.describe_assets).parameters["workers"].default)
        build_source = Path("scripts/rag_v2/build.py").read_text(encoding="utf-8")
        self.assertIn('"--vl-workers", type=int, default=1', build_source)

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


if __name__ == "__main__":
    unittest.main()
