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

    def test_disk_billing_patterns_require_local_context_and_cross_area(self):
        unrelated = _question_patterns(
            "XFTP上传", ["数据上传", "XFTP上传"], "billing_rule", "operation/upload.md",
            "XFTP 软件可申请免费许可。" + "连接配置说明。" * 30 + "稍后截图还显示系统盘 200 GB。",
        )
        self.assertNotIn("系统盘100GB为什么还收费", unrelated)
        announcement = _question_patterns(
            "GPU 新功能发布记录", ["GPU 新功能发布记录"], "resource_purchase", "overview/update-gpu.md",
            "新实例系统盘免费额度从200G降至100G；旧实例扩容后也按100G重新计费。",
        )
        self.assertIn("系统盘100GB为什么还收费", announcement)

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
