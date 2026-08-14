import json
from pathlib import Path
import tempfile
import unittest

from scripts.rag_w0.build_corpus_embeddings import write_sidecar

from scripts.rag_v2.pipeline import (
    AssetNote,
    SourceDocument,
    build_chunks,
    clean_public_text,
    document_type,
    inject_asset_notes,
    normalize_image_markup,
    merge_external,
    plan_document_units,
    semantic_parts,
    validate_chunks,
    _canonical_remote_image_url,
    _image_content_type,
    _is_decorative_asset,
    _prepare_vl_image,
    _retryable_asset_failure,
    _question_patterns,
)


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
