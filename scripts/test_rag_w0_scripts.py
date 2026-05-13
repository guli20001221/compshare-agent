import json
from pathlib import Path
import sys
import tempfile
import unittest

SCRIPTS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS_DIR))

from rag_w0 import build_source_manifest
from rag_w0 import classify_links
from rag_w0 import validate_source
from rag_w0 import extract_assets
from rag_w0 import validate_chunks


class RagW0ScriptTests(unittest.TestCase):
    def test_build_source_manifest_validates_paths_and_spt_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            docs = root / "docs"
            docs.mkdir()
            (docs / "faq.md").write_text("# FAQ\n", encoding="utf-8")
            internal = root / "internal_cases"
            internal.mkdir()
            spt = internal / "spt-record.txt"
            spt.write_text("internal case export", encoding="utf-8")
            manifest_path = root / "manifest.json"
            manifest_path.write_text(
                json.dumps(
                    {
                        "bundle_root": str(root),
                        "sources": [
                            {
                                "id": "docs",
                                "type": "gitlab_clone_subset",
                                "path": str(docs),
                                "include_status": "include_with_directory_policy",
                                "customer_safe": "mixed",
                            },
                            {
                                "id": "wxwork-spt-record-2026-05",
                                "type": "internal_case_chat_export",
                                "path": str(spt),
                                "include_status": "internal_reference_only_needs_customer_safe_split",
                                "customer_safe": "false",
                            },
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8-sig",
            )

            out = build_source_manifest.build_source_manifest(manifest_path)

            self.assertEqual(out["schema_version"], "rag_w0.source_manifest.v1")
            self.assertEqual(out["source_count"], 2)
            docs_entry = next(item for item in out["sources"] if item["id"] == "docs")
            self.assertEqual(docs_entry["file_count"], 1)
            self.assertGreater(docs_entry["byte_count"], 0)
            spt_entry = next(item for item in out["sources"] if item["id"] == "wxwork-spt-record-2026-05")
            self.assertEqual(spt_entry["customer_safe"], "false")

            broken = json.loads(manifest_path.read_text(encoding="utf-8-sig"))
            broken["sources"][0]["path"] = str(root / "missing")
            manifest_path.write_text(json.dumps(broken), encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "missing source path"):
                build_source_manifest.build_source_manifest(manifest_path)

    def test_validate_source_manifest_independently_enforces_spt_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            spt = root / "spt-record.txt"
            spt.write_text("internal case export", encoding="utf-8")
            manifest = {
                "schema_version": "rag_w0.source_manifest.v1",
                "sources": [
                    {
                        "id": "wxwork-spt-record-2026-05",
                        "type": "internal_case_chat_export",
                        "include_status": "internal_reference_only_needs_customer_safe_split",
                        "customer_safe": "false",
                        "paths": {"path": str(spt)},
                    }
                ],
            }
            path = root / "source_manifest.json"
            path.write_text(json.dumps(manifest, ensure_ascii=False), encoding="utf-8")

            self.assertEqual(validate_source.validate_source_manifest(path), {"source_count": 1})

            broken = json.loads(path.read_text(encoding="utf-8"))
            broken["sources"][0]["type"] = "feishu_docx_raw"
            path.write_text(json.dumps(broken), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "type must be internal_case_chat_export"):
                validate_source.validate_source_manifest(path)

            broken["sources"][0]["type"] = "internal_case_chat_export"
            broken["sources"][0]["include_status"] = "include_after_cleaning"
            path.write_text(json.dumps(broken), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "include_status"):
                validate_source.validate_source_manifest(path)

    def test_extract_assets_finds_markdown_images_links_and_marks_final_states(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "guide.md"
            image = root / "step.png"
            image.write_bytes(b"fake image bytes")
            doc.write_text(
                "\n".join(
                    [
                        "# Login",
                        "Follow [console](https://console.compshare.cn/instances).",
                        "Unknown [external](https://example.com/help).",
                        "![step](step.png)",
                        "![missing](missing.png)",
                    ]
                ),
                encoding="utf-8",
            )
            source_manifest = {
                "sources": [
                    {
                        "id": "guide",
                        "type": "feishu_docx_raw",
                        "paths": {"path": str(doc)},
                    }
                ]
            }

            asset_manifest, link_manifest = extract_assets.extract_assets_and_links(source_manifest)

            states = {asset["image_ref"]: asset["final_state"] for asset in asset_manifest["assets"]}
            self.assertEqual(states["step.png"], "included_with_ocr_note")
            self.assertEqual(states["missing.png"], "missing_asset")
            self.assertEqual(len(link_manifest["links"]), 2)
            self.assertTrue(all(link["final_state"] == "unknown" for link in link_manifest["links"]))
            self.assertTrue(all(link["link_type"] == "unknown" for link in link_manifest["links"]))

    def test_classify_links_applies_deterministic_policy(self):
        link_manifest = {
            "links": [
                {"link_id": "1", "url": "https://console.compshare.cn/instances"},
                {"link_id": "2", "url": "https://gitlab.example.com/group/project"},
                {"link_id": "3", "url": "https://www.compshare.cn/docs/gpus/login"},
                {"link_id": "4", "url": "https://example.com/download?token=abc"},
            ]
        }

        classified = classify_links.classify_link_manifest(link_manifest)
        by_id = {link["link_id"]: link for link in classified["links"]}

        self.assertEqual(by_id["1"]["link_type"], "official_console_route")
        self.assertEqual(by_id["1"]["final_state"], "navigation_only")
        self.assertEqual(by_id["2"]["link_type"], "internal_source_provenance")
        self.assertEqual(by_id["2"]["final_state"], "excluded")
        self.assertEqual(by_id["3"]["link_type"], "public_official_docs")
        self.assertEqual(by_id["3"]["final_state"], "review_required")
        self.assertEqual(by_id["4"]["link_type"], "temporary_download")
        self.assertEqual(by_id["4"]["final_state"], "excluded")

    def test_validate_chunks_enforces_reserved_fields_and_surface_url_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "chunks.jsonl"
            chunk = {
                "chunk_id": "w0-init-failure-001",
                "kb_version": "kb.stage2b.w0.2026-05-13",
                "source_type": "faq",
                "product_area": "init_failure",
                "acl": "customer_safe",
                "title": "Init failure",
                "question_patterns": ["初始化失败怎么办"],
                "content": "先查看控制台状态，再联系平台客服确认。",
                "source_refs": ["guide"],
                "asset_refs": [],
                "confidence": "high",
                "valid_from": "2026-05-13",
                "evidence_kind": "knowledge",
                "surface_url": None,
                "retrieval_score_hint": None,
            }
            path.write_text(json.dumps(chunk, ensure_ascii=False) + "\n", encoding="utf-8")

            summary = validate_chunks.validate_chunks(path)

            self.assertEqual(summary["chunk_count"], 1)

            invalid_urls = [
                ("http://console.compshare.cn/instances", "scheme_not_https"),
                ("https://www.compshare.cn/pricing", "host_not_in_allowlist"),
                ("https://gitlab.example.com/group/project", "denied_internal_host"),
                ("https://foo.feishu.cn/docx/abc", "denied_internal_host"),
                ("https://console.compshare.cn/admin/users", "internal_path"),
                ("https://console.compshare.cn/instances?token=abc", "signed_url_query"),
                ("https://download.compshare.cn/file?expires=1", "temporary_download"),
            ]
            for url, reason in invalid_urls:
                with self.subTest(url=url):
                    bad = dict(chunk, surface_url=url)
                    path.write_text(json.dumps(bad, ensure_ascii=False) + "\n", encoding="utf-8")
                    with self.assertRaisesRegex(ValueError, reason):
                        validate_chunks.validate_chunks(path)

            valid_business_query = dict(chunk, surface_url="https://console.compshare.cn/instances?e=event")
            path.write_text(json.dumps(valid_business_query, ensure_ascii=False) + "\n", encoding="utf-8")
            self.assertEqual(validate_chunks.validate_chunks(path)["chunk_count"], 1)

            valid_attempt_host = dict(chunk, surface_url="https://attempt.example.com/docs/page")
            path.write_text(json.dumps(valid_attempt_host, ensure_ascii=False) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "host_not_in_allowlist"):
                validate_chunks.validate_chunks(path)

            bad = dict(chunk, evidence_kind="diagnosis")
            path.write_text(json.dumps(bad, ensure_ascii=False) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "evidence_kind"):
                validate_chunks.validate_chunks(path)

            case_without_approval = dict(chunk, source_refs=["wxwork-spt-record-2026-05:case-1"])
            path.write_text(json.dumps(case_without_approval, ensure_ascii=False) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "approval record"):
                validate_chunks.validate_chunks(path)

            future_case_without_approval = dict(chunk, source_refs=["wxwork-spt-record-2026-06:case-1"])
            path.write_text(json.dumps(future_case_without_approval, ensure_ascii=False) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "approval record"):
                validate_chunks.validate_chunks(path)

            future_case_with_approval = dict(
                chunk,
                source_refs=["wxwork-spt-record-2026-06:case-1"],
                approval_record_hash="sha256:approved",
            )
            path.write_text(json.dumps(future_case_with_approval, ensure_ascii=False) + "\n", encoding="utf-8")
            self.assertEqual(validate_chunks.validate_chunks(path)["chunk_count"], 1)


if __name__ == "__main__":
    unittest.main()
