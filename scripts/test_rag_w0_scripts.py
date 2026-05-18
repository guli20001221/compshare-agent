import json
import contextlib
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

SCRIPTS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS_DIR))

from rag_w0 import build_source_manifest
from rag_w0 import clean_docs
from rag_w0 import check_internal_leakage
from rag_w0 import chunk_docs
from rag_w0 import classify_links
from rag_w0 import common
from rag_w0 import describe_images
from rag_w0 import evaluate_answers
from rag_w0 import evaluate_retrieval
from rag_w0 import extract_assets
from rag_w0 import generate_eval_questions
from rag_w0 import label_sections
from rag_w0 import mine_internal_cases
from rag_w0 import model_smoke
from rag_w0 import normalize_docs
from rag_w0 import parse_sections
from rag_w0 import retrieval_scoring
from rag_w0 import safety_patterns
from rag_w0 import chunk_plan
from rag_w0 import select_w0_sources
from rag_w0 import snapshot_assets
from rag_w0 import snapshot_links
from rag_w0 import validate_case_approvals
from rag_w0 import validate_chunks
from rag_w0 import validate_cleaned_docs
from rag_w0 import validate_source
from rag_w0 import verify_chunk_plan_anchors
from rag_w0 import verify_eval_questions
from rag_w0 import verify_section_lists
from rag_w0 import verify_pinned_sections
from rag_w0 import write_eval_report


class RagW0ScriptTests(unittest.TestCase):
    def _write_cleaned_doc(self, root: Path, body: str, name: str = "usage-faq.md") -> Path:
        cleaned = root / "cleaned"
        cleaned.mkdir(exist_ok=True)
        path = cleaned / name
        path.write_text(
            "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
            + body,
            encoding="utf-8",
        )
        return cleaned

    def _chunk_text(self, body: str, *, require_complete_inputs: bool = False, max_chars: int = 2000) -> list[dict]:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, body)
            out = root / "chunks.jsonl"
            kwargs = {}
            if require_complete_inputs:
                asset_notes = root / "asset_notes.jsonl"
                asset_notes.write_text(
                    json.dumps(
                        {
                            "asset_id": "asset-1",
                            "include_in_rag": True,
                            "final_state": "included_with_vl_note",
                            "requires_review": False,
                            "description": "VL description",
                            "model_metadata": {"vl_executed": True},
                        },
                        ensure_ascii=False,
                    )
                    + "\n",
                    encoding="utf-8",
                )
                links = root / "links.json"
                links.write_text(json.dumps({"links": []}, ensure_ascii=False), encoding="utf-8")
                kwargs.update(asset_notes_path=asset_notes, link_manifest_path=links)
            chunk_docs.chunk_documents(
                cleaned,
                out,
                kb_version="kb.test",
                valid_from="2026-05-13",
                require_complete_inputs=require_complete_inputs,
                max_chars=max_chars,
                **kwargs,
            )
            return [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]

    def _chunk_cleaned_doc(self, front_matter: list[str], body: str, *, name: str = "guide.md") -> list[dict]:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / name).write_text("---\n" + "\n".join(front_matter) + "\n---\n" + body, encoding="utf-8")
            out = root / "chunks.jsonl"
            chunk_docs.chunk_documents(
                cleaned,
                out,
                kb_version="kb.test",
                valid_from="2026-05-13",
                require_complete_inputs=False,
            )
            return [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]

    def _valid_chunk(self, **overrides) -> dict:
        chunk = {
            "chunk_id": "w0-login-doc-001",
            "kb_version": "kb.test",
            "source_type": "faq",
            "product_area": "login",
            "acl": "customer_safe",
            "title": "Login",
            "question_patterns": ["How to login"],
            "content": "Open the console and connect to the instance.",
            "source_refs": ["doc"],
            "asset_refs": [],
            "confidence": "high",
            "valid_from": "2026-05-13",
            "evidence_kind": "knowledge",
            "surface_url": None,
            "retrieval_score_hint": None,
        }
        chunk.update(overrides)
        return chunk

    def _write_normalized_doc(self, root: Path, name: str, *, source_id: str, include_status: str, source_path: str, body: str = "# Doc\nCustomer-facing guidance.\n") -> Path:
        normalized = root / "normalized"
        normalized.mkdir(exist_ok=True)
        path = normalized / name
        path.write_text(
            "\n".join(
                [
                    "---",
                    f"source_id: {source_id}",
                    "source_type: gitlab_clone_subset" if source_id == "gitlab-compshare-docs" else "source_type: feishu_docx_raw",
                    "safety_state: mixed" if source_id == "gitlab-compshare-docs" else "safety_state: needs_cleaning",
                    f"include_status: {include_status}",
                    f"source_path: {source_path}",
                    "---",
                    "",
                    body,
                ]
            ),
            encoding="utf-8",
        )
        return normalized

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

    def test_select_w0_sources_uses_explicit_allowlist_for_gitlab_docs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = self._write_normalized_doc(
                root,
                "feishu-usage__faq.md",
                source_id="feishu-usage-faq-latest",
                include_status="include_after_cleaning",
                source_path="F:/bundle/feishu/faq.md",
                body="# Feishu FAQ\nExisting customer FAQ stays in scope.\n",
            )
            self._write_normalized_doc(
                root,
                "gitlab-compshare-docs__logininstance.md",
                source_id="gitlab-compshare-docs",
                include_status="include_with_directory_policy",
                source_path="F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                body="# Login\nSSH and Jupyter login guidance.\n",
            )
            self._write_normalized_doc(
                root,
                "gitlab-compshare-docs__event0.md",
                source_id="gitlab-compshare-docs",
                include_status="include_with_directory_policy",
                source_path="F:/bundle/gitlab-compshare-docs/pages/overview/specialevent/event0.md",
                body="# Event\nMarketing content should not enter W0.\n",
            )
            allowlist = root / "allowlist.json"
            allowlist.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0_gitlab_source_allowlist.v1",
                        "sources": [
                            {
                                "source_id": "gitlab-compshare-docs",
                                "doc_path": "pages/operation/gpu/logininstance.md",
                                "product_area": "login",
                                "reason": "SSH and Jupyter login guidance",
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            out = root / "selected"

            summary = select_w0_sources.select_w0_sources(normalized, allowlist, out)

            selected_names = sorted(path.name for path in out.glob("*.md"))
            self.assertEqual(selected_names, ["feishu-usage__faq.md", "gitlab-compshare-docs__logininstance.md"])
            gitlab_text = (out / "gitlab-compshare-docs__logininstance.md").read_text(encoding="utf-8")
            self.assertIn("include_status: include_after_cleaning", gitlab_text)
            self.assertIn("source_selection_product_area: login", gitlab_text)
            self.assertEqual(summary["copied_existing_count"], 1)
            self.assertEqual(summary["selected_gitlab_count"], 1)
            self.assertEqual(summary["skipped_gitlab_count"], 1)
            self.assertEqual(summary["product_area_counts"], {"login": 1})
            self.assertEqual(
                sorted(summary["selected_source_paths"]),
                [
                    "F:/bundle/feishu/faq.md",
                    "F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                ],
            )

    def test_select_w0_sources_can_filter_assets_and_links_to_selected_docs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = self._write_normalized_doc(
                root,
                "gitlab-compshare-docs__logininstance.md",
                source_id="gitlab-compshare-docs",
                include_status="include_with_directory_policy",
                source_path="F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                body="# Login\n![login](login.png)\n",
            )
            self._write_normalized_doc(
                root,
                "gitlab-compshare-docs__us3fileoperation.md",
                source_id="gitlab-compshare-docs",
                include_status="include_with_directory_policy",
                source_path="F:/bundle/gitlab-compshare-docs/pages/gpus/data/us3fileoperation.md",
                body="# US3\n[external](https://docs.ucloud.cn/ufile/tools/us3cli/prepare)\n",
            )
            allowlist = root / "allowlist.json"
            allowlist.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0_gitlab_source_allowlist.v1",
                        "sources": [
                            {
                                "source_id": "gitlab-compshare-docs",
                                "doc_path": "pages/operation/gpu/logininstance.md",
                                "product_area": "login",
                                "reason": "required for W0",
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            links = root / "links.json"
            links.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0.links.v1",
                        "links": [
                            {
                                "link_id": "keep",
                                "source_id": "gitlab-compshare-docs",
                                "source_path": "F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                                "final_state": "local_source_resolved",
                            },
                            {
                                "link_id": "drop",
                                "source_id": "gitlab-compshare-docs",
                                "source_path": "F:/bundle/gitlab-compshare-docs/pages/gpus/data/us3fileoperation.md",
                                "final_state": "review_required",
                            },
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            assets = root / "assets.json"
            assets.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0.assets.v1",
                        "assets": [
                            {
                                "asset_id": "asset-keep",
                                "source_id": "gitlab-compshare-docs",
                                "source_doc_id": "F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                            },
                            {
                                "asset_id": "asset-drop",
                                "source_id": "gitlab-compshare-docs",
                                "source_doc_id": "F:/bundle/gitlab-compshare-docs/pages/gpus/data/us3fileoperation.md",
                            },
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            select_w0_sources.select_w0_sources(
                normalized,
                allowlist,
                root / "selected",
                links_path=links,
                links_out_path=root / "selected_links.json",
                assets_path=assets,
                assets_out_path=root / "selected_assets.json",
            )

            selected_links = json.loads((root / "selected_links.json").read_text(encoding="utf-8"))
            selected_assets = json.loads((root / "selected_assets.json").read_text(encoding="utf-8"))
            self.assertEqual([link["link_id"] for link in selected_links["links"]], ["keep"])
            self.assertEqual(selected_links["link_count"], 1)
            self.assertEqual([asset["asset_id"] for asset in selected_assets["assets"]], ["asset-keep"])
            self.assertEqual(selected_assets["asset_count"], 1)

    def test_select_w0_sources_fails_when_allowlisted_doc_is_missing(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = self._write_normalized_doc(
                root,
                "gitlab-compshare-docs__other.md",
                source_id="gitlab-compshare-docs",
                include_status="include_with_directory_policy",
                source_path="F:/bundle/gitlab-compshare-docs/pages/operation/gpu/other.md",
            )
            allowlist = root / "allowlist.json"
            allowlist.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0_gitlab_source_allowlist.v1",
                        "sources": [
                            {
                                "source_id": "gitlab-compshare-docs",
                                "doc_path": "pages/operation/gpu/logininstance.md",
                                "product_area": "login",
                                "reason": "required for W0",
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "allowlisted docs were not found"):
                select_w0_sources.select_w0_sources(normalized, allowlist, root / "selected")

    def test_select_w0_sources_validates_allowlist_product_area(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = root / "normalized"
            normalized.mkdir()
            allowlist = root / "allowlist.json"
            allowlist.write_text(
                json.dumps(
                    {
                        "schema_version": "rag_w0_gitlab_source_allowlist.v1",
                        "sources": [
                            {
                                "source_id": "gitlab-compshare-docs",
                                "doc_path": "pages/operation/gpu/logininstance.md",
                                "product_area": "marketing",
                                "reason": "bad area",
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "invalid product_area"):
                select_w0_sources.select_w0_sources(normalized, allowlist, root / "selected")

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
                {"link_id": "5", "url": "mailto:support@example.com"},
                {"link_id": "6", "url": "#section"},
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
        self.assertEqual(by_id["5"]["link_type"], "non_http_scheme")
        self.assertEqual(by_id["5"]["final_state"], "excluded")
        self.assertEqual(by_id["6"]["link_type"], "local_source_candidate")
        self.assertEqual(by_id["6"]["final_state"], "local_source_resolved")

    def test_surface_url_policy_aligns_runtime_contract(self):
        self.assertIsNone(common.surface_url_rejection_reason("https://console.compshare.cn:443/instances"))
        self.assertEqual(common.surface_url_rejection_reason("https://"), "invalid_url")
        self.assertEqual(common.surface_url_rejection_reason("https://tmp.example.com/file"), "temporary_download")
        self.assertEqual(common.surface_url_rejection_reason("https://temporary.example.com/file"), "temporary_download")
        self.assertEqual(common.surface_url_rejection_reason("https://download.example.com/file?x=1"), "temporary_download")
        self.assertEqual(common.surface_url_rejection_reason("https://files.download:8080/file?x=1"), "temporary_download")
        self.assertEqual(common.surface_url_rejection_reason("https://attempt.example.com/docs/page"), "host_not_in_allowlist")

    def test_describe_images_accounts_for_every_asset(self):
        asset_manifest = {
            "assets": [
                {
                    "asset_id": "asset-1",
                    "source_id": "guide",
                    "source_doc_id": "guide.md",
                    "image_ref": "step.png",
                    "image_path": "step.png",
                    "heading_path": ["Login"],
                    "nearby_text": "Click the console login button, then enter the password.",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "exclusion_reason": "",
                },
                {
                    "asset_id": "asset-2",
                    "source_id": "guide",
                    "source_doc_id": "guide.md",
                    "image_ref": "logo.png",
                    "image_path": "logo.png",
                    "heading_path": [],
                    "nearby_text": "",
                    "final_state": "excluded_low_value",
                    "include_in_rag": False,
                    "exclusion_reason": "low_value",
                },
            ]
        }

        notes = describe_images.describe_asset_notes(asset_manifest)
        by_id = {note["asset_id"]: note for note in notes}

        self.assertEqual(set(by_id), {"asset-1", "asset-2"})
        for note in notes:
            self.assertIn("source_doc_id", note)
            self.assertIn("heading_path", note)
            self.assertIn("nearby_text", note)
            self.assertIn("exclusion_reason", note)
            self.assertIn(
                note["visual_type"],
                {"operation_screenshot", "error_screenshot", "console_state", "diagram", "logo", "qr_code", "decorative", "unknown"},
            )
            self.assertFalse(note["model_metadata"]["vl_executed"])
        self.assertEqual(by_id["asset-1"]["note_type"], "operation_note")
        self.assertEqual(by_id["asset-1"]["user_action"], "Click the console login button, then enter the password.")
        self.assertTrue(by_id["asset-1"]["requires_review"])
        self.assertEqual(by_id["asset-1"]["model_metadata"]["method"], "deterministic_nearby_text")
        self.assertEqual(by_id["asset-2"]["note_type"], "excluded")
        self.assertEqual(by_id["asset-2"]["visual_type"], "logo")
        self.assertFalse(by_id["asset-2"]["include_in_rag"])

    def test_snapshot_links_records_success_and_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            link_manifest = {
                "links": [
                    {
                        "link_id": "ok",
                        "url": "https://www.compshare.cn/docs/gpus/login",
                        "link_type": "public_official_docs",
                        "final_state": "review_required",
                    },
                    {
                        "link_id": "bad",
                        "url": "https://example.com/missing",
                        "link_type": "unknown_external",
                        "final_state": "review_required",
                    },
                    {
                        "link_id": "nav",
                        "url": "https://console.compshare.cn/instances",
                        "link_type": "official_console_route",
                        "final_state": "navigation_only",
                    },
                ]
            }

            def fetcher(url):
                if url.endswith("/login"):
                    return b"<html>login docs</html>", "text/html"
                raise RuntimeError("network blocked")

            out = snapshot_links.snapshot_link_manifest(link_manifest, root, fetcher=fetcher)
            by_id = {link["link_id"]: link for link in out["links"]}

            self.assertEqual(by_id["ok"]["final_state"], "snapshotted")
            self.assertEqual(by_id["ok"]["snapshot_status"], "success")
            self.assertTrue((root / by_id["ok"]["snapshot_path"]).exists())
            self.assertRegex(by_id["ok"]["snapshot_sha256"], r"^[0-9a-f]{64}$")
            self.assertEqual(by_id["bad"]["final_state"], "review_required")
            self.assertEqual(by_id["bad"]["snapshot_status"], "failed")
            self.assertEqual(by_id["bad"]["snapshot_failure_reason"], "network blocked")
            self.assertNotIn("snapshot_status", by_id["nav"])

    def test_snapshot_assets_downloads_external_images_for_vl(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = {
                "schema_version": "rag_w0.assets.v1",
                "assets": [
                    {
                        "asset_id": "external-a",
                        "source_id": "gitlab-compshare-docs",
                        "source_doc_id": "F:/bundle/gitlab-compshare-docs/pages/operation/gpu/logininstance.md",
                        "image_ref": "https://www-s.ucloud.cn/path/login.png",
                        "image_path": None,
                        "final_state": "external_asset_snapshot_required",
                        "include_in_rag": False,
                    },
                    {
                        "asset_id": "local-b",
                        "source_id": "runbook",
                        "source_doc_id": "F:/bundle/runbook.md",
                        "image_ref": "images/local.png",
                        "image_path": "F:/bundle/images/local.png",
                        "final_state": "included_with_ocr_note",
                        "include_in_rag": True,
                    },
                ],
            }

            def fetcher(url: str) -> tuple[bytes, str]:
                self.assertEqual(url, "https://www-s.ucloud.cn/path/login.png")
                return b"\x89PNG\r\n\x1a\nfake", "image/png"

            updated = snapshot_assets.snapshot_external_assets(manifest, root / "asset_snapshots", fetcher=fetcher)

            external = updated["assets"][0]
            self.assertEqual(external["final_state"], "included_with_ocr_note")
            self.assertTrue(external["include_in_rag"])
            self.assertEqual(external["snapshot_status"], "success")
            self.assertEqual(external["snapshot_content_type"], "image/png")
            self.assertTrue(Path(external["image_path"]).exists())
            self.assertEqual(Path(external["image_path"]).suffix, ".png")
            self.assertEqual(updated["snapshot_success_count"], 1)
            self.assertEqual(updated["snapshot_failure_count"], 0)
            self.assertEqual(updated["assets"][1]["image_path"], "F:/bundle/images/local.png")

    def test_extract_and_normalize_handle_multiline_external_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "guide.md"
            doc.write_text(
                "# Image guide\n"
                "Before\n"
                "![deploy](\n"
                "https://www-s.ucloud.cn/path/deploy.png)\n"
                "After\n",
                encoding="utf-8",
            )
            source_manifest = {
                "sources": [
                    {
                        "id": "guide",
                        "type": "gitlab_clone_subset",
                        "customer_safe": "mixed",
                        "include_status": "include_after_cleaning",
                        "paths": {"path": str(doc)},
                    }
                ]
            }

            asset_manifest, _ = extract_assets.extract_assets_and_links(source_manifest)

            self.assertEqual(asset_manifest["asset_count"], 1)
            asset = asset_manifest["assets"][0]
            self.assertEqual(asset["final_state"], "external_asset_snapshot_required")
            self.assertEqual(asset["line"], 3)
            self.assertEqual(asset["image_ref"], "https://www-s.ucloud.cn/path/deploy.png")

            asset["final_state"] = "included_with_vl_note"
            asset["include_in_rag"] = True
            asset["image_path"] = str(root / "deploy.png")
            notes = [
                {
                    "asset_id": asset["asset_id"],
                    "include_in_rag": True,
                    "visual_type": "operation_screenshot",
                    "description": "Deploy button screenshot",
                    "highlighted_ui": "Deploy",
                    "user_action": "Click deploy",
                    "next_step": "Wait for instance",
                    "confidence": "high",
                    "final_state": "included_with_vl_note",
                    "requires_review": False,
                    "model_metadata": {"vl_executed": True},
                }
            ]
            out_dir = root / "normalized"

            normalize_docs.normalize_documents(source_manifest, asset_manifest, {"links": []}, notes, out_dir)

            text = next(out_dir.glob("*.md")).read_text(encoding="utf-8")
            self.assertIn("<!-- asset_note:", text)
            self.assertIn("Before", text)
            self.assertIn("After", text)
            self.assertNotIn("![deploy]", text)

    def test_normalize_docs_inserts_asset_notes_and_front_matter(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "guide.md"
            image = root / "step.png"
            image.write_bytes(b"image")
            doc.write_text("---\nnode_token: secret\n---\n# Login\nBefore\n![step](step.png)\nAfter\n", encoding="utf-8")
            source_manifest = {
                "sources": [
                    {
                        "id": "guide",
                        "type": "feishu_docx_raw",
                        "customer_safe": "needs_cleaning",
                        "include_status": "include_after_cleaning",
                        "paths": {"path": str(doc)},
                    }
                ]
            }
            asset_manifest, link_manifest = extract_assets.extract_assets_and_links(source_manifest)
            notes = describe_images.describe_asset_notes(asset_manifest)
            out_dir = root / "normalized"

            result = normalize_docs.normalize_documents(source_manifest, asset_manifest, {"links": []}, notes, out_dir)

            self.assertEqual(result["normalized_count"], 1)
            normalized = next(out_dir.glob("*.md"))
            text = normalized.read_text(encoding="utf-8")
            self.assertIn("source_id: guide", text)
            self.assertIn("safety_state: needs_cleaning", text)
            self.assertIn("<!-- asset_note:", text)
            self.assertIn("After", text)
            self.assertEqual(link_manifest["link_count"], 0)

    def test_normalize_docs_includes_new_asset_note_fields(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "guide.md"
            image = root / "step.png"
            image.write_bytes(b"image")
            doc.write_text("# Login\nBefore\n![step](step.png)\nAfter\n", encoding="utf-8")
            source_manifest = {
                "sources": [
                    {
                        "id": "guide",
                        "type": "feishu_docx_raw",
                        "customer_safe": "needs_cleaning",
                        "include_status": "include_after_cleaning",
                        "paths": {"path": str(doc)},
                    }
                ]
            }
            asset_manifest, _ = extract_assets.extract_assets_and_links(source_manifest)
            notes = describe_images.describe_asset_notes(asset_manifest)
            out_dir = root / "normalized"

            normalize_docs.normalize_documents(source_manifest, asset_manifest, {"links": []}, notes, out_dir)

            text = next(out_dir.glob("*.md")).read_text(encoding="utf-8")
            match = re.search(r"<!--\s*asset_note:\s*(\{.*?\})\s*-->", text)
            self.assertIsNotNone(match)
            payload = json.loads(match.group(1))
            self.assertIs(payload["include_in_rag"], True)
            self.assertIn(payload["visual_type"], describe_images.VISUAL_TYPES)
            self.assertIn("highlighted_ui", payload)

    def test_clean_docs_redacts_internal_content_and_skips_non_whitelisted_sources(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = root / "normalized"
            normalized.mkdir()
            public_doc = normalized / "guide.md"
            public_doc.write_text(
                "---\nsource_id: guide\nsource_type: faq\nsafety_state: needs_cleaning\ninclude_status: include_after_cleaning\n---\n"
                "---\nnode_token: abc123\n---\n"
                "Contact \u5f20\u6167 and \u5f20\u96e8\u6b23, instance uhost-abc123, password=secret, "
                "Bearer abc, api_key=abc, \u5bc6\u7801\uff1acompshare123, \u53e3\u4ee4\uff1ademo123, see https://gitlab.example.com/a, "
                "use /cloud/xxx/snapshotter/snapshots/3/fs, \u975e\u6807\u6302\u76d8, SPT\u5de5\u5177, \u7f57\u76d8, "
                "\u8054\u7cfbSRE, \u8054\u7cfb\u7814\u53d1, \u8054\u7cfb\u8fd0\u8425(\u5f20\u6167/\u5f20\u96e8\u6b23), @\u4f18\u4e91\u667a\u7b97QA\u5c0f\u52a9\u624b.\n",
                encoding="utf-8",
            )
            internal_doc = normalized / "case.md"
            internal_doc.write_text(
                "---\nsource_id: case-doc\nsource_type: converted_pdf_or_internal_runbook_zip\nsafety_state: needs_cleaning\ninclude_status: internal_reference_only_needs_customer_safe_split\n---\nraw\n",
                encoding="utf-8",
            )
            out_dir = root / "cleaned"

            result = clean_docs.clean_documents(normalized, out_dir)

            self.assertEqual(result["cleaned_count"], 1)
            self.assertEqual(result["skipped_count"], 1)
            cleaned_text = (out_dir / "guide.md").read_text(encoding="utf-8")
            for forbidden in [
                "uhost-abc123",
                "password=secret",
                "node_token",
                "Bearer abc",
                "api_key=abc",
                "\u5bc6\u7801\uff1acompshare123",
                "\u53e3\u4ee4\uff1ademo123",
                "gitlab.example.com",
                "/cloud/xxx",
                "\u5f20\u6167",
                "\u5f20\u96e8\u6b23",
                "\u975e\u6807",
                "SPT\u5de5\u5177",
                "\u7f57\u76d8",
                "\u8054\u7cfbSRE",
                "\u8054\u7cfb\u7814\u53d1",
                "@\u4f18\u4e91\u667a\u7b97QA\u5c0f\u52a9\u624b",
            ]:
                self.assertNotIn(forbidden, cleaned_text)
            self.assertNotIn("source_id: guide", cleaned_text)
            self.assertIn("source_trace_hash:", cleaned_text)
            self.assertFalse((out_dir / "case.md").exists())
            self.assertEqual(validate_cleaned_docs.validate_cleaned_docs(out_dir), {"checked_count": 1})

    def test_validate_cleaned_docs_fails_on_customer_unsafe_patterns(self):
        patterns = [
            "uhost-abc123",
            "bsi-0ddf786952\u4e91\u76d8\u6269\u5bb9",
            "_uhost_uhost-1oxs60xxkbeg",
            "node_token: abc",
            "\u5bc6\u7801\uff1acompshare123",
            "\u53e3\u4ee4\uff1ademo123",
            "Bearer abc",
            "api_key=abc",
            "https://gitlab.example.com/a",
            "/cloud/xxx/snapshotter/snapshots/3/fs",
            "\u5f20\u6167",
            "(qi.li)",
            "@\u5f20\u6167",
            "\u975e\u6807\u6302\u76d8",
            "SPT\u5de5\u5177",
            "\u7f57\u76d8",
            "\u8054\u7cfbSRE",
            "\u8054\u7cfb\u7814\u53d1",
            "@\u4f18\u4e91\u667a\u7b97QA\u5c0f\u52a9\u624b",
        ]
        for pattern in patterns:
            with self.subTest(pattern=pattern), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                doc = root / "dirty.md"
                doc.write_text(f"---\nsafety_state: customer_safe_cleaned\n---\n{pattern}\n", encoding="utf-8")
                with self.assertRaisesRegex(ValueError, "unsafe cleaned doc"):
                    validate_cleaned_docs.validate_cleaned_docs(root)

    def test_safety_patterns_allow_documented_bearer_placeholders(self):
        safe_examples = [
            "Authorization Bearer api_key 怎么传",
            "Authorization Bearer 怎么传",
            "Authorization Bearer max_tokens 必填吗",
            "Authorization Bearer <YOUR_API_KEY>",
        ]
        for example in safe_examples:
            with self.subTest(example=example):
                self.assertNotIn("secret", safety_patterns.unsafe_cleaned_matches(example))

        for unsafe in ["Bearer abc", "Bearer tokenabc", "api_key=abc"]:
            with self.subTest(unsafe=unsafe):
                self.assertIn("secret", safety_patterns.unsafe_cleaned_matches(unsafe))

    def test_safety_patterns_do_not_treat_capacity_ranges_as_wechat_handles(self):
        self.assertNotIn("wechat_handle", safety_patterns.unsafe_cleaned_matches("数据盘容量(10G-1500G)"))
        self.assertIn("wechat_handle", safety_patterns.unsafe_cleaned_matches("请联系(qi.li)处理"))

    def test_clean_docs_redacts_asset_note_values_without_breaking_json(self):
        body = (
            'Before <!-- asset_note: {"asset_id":"asset-1","description":"密码: *****",'
            '"include_in_rag":true,"visual_type":"console_state","user_action":"输入 uhost-abc123"} --> after\n'
        )

        cleaned, needs_review = clean_docs.clean_text(body)

        self.assertFalse(needs_review)
        match = re.search(r"<!--\s*asset_note:\s*(\{.*?\})\s*-->", cleaned, flags=re.DOTALL)
        self.assertIsNotNone(match)
        payload = json.loads(match.group(1))
        self.assertEqual(payload["asset_id"], "asset-1")
        self.assertEqual(payload["description"], "[SECRET_REDACTED]")
        self.assertEqual(payload["user_action"], "输入 [RESOURCE_ID_REDACTED]")

    def test_clean_docs_preserves_selected_product_area(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = root / "normalized"
            normalized.mkdir()
            (normalized / "error-code.md").write_text(
                "---\n"
                "source_id: gitlab-compshare-docs\n"
                "source_type: gitlab_clone_subset\n"
                "safety_state: mixed\n"
                "include_status: include_after_cleaning\n"
                "source_path: F:/bundle/pages/gpus/compshareerrorcode.md\n"
                "source_selection_product_area: init_failure\n"
                "---\n"
                "# Error codes\n"
                "Instance init failed after arrears were resolved.\n",
                encoding="utf-8",
            )
            out = root / "cleaned"

            clean_docs.clean_documents(normalized, out)

            cleaned = (out / "error-code.md").read_text(encoding="utf-8")
            self.assertIn("source_selection_product_area: init_failure", cleaned)

    def test_clean_docs_omits_missing_selected_product_area(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            normalized = root / "normalized"
            normalized.mkdir()
            (normalized / "guide.md").write_text(
                "---\n"
                "source_id: guide\n"
                "source_type: faq\n"
                "safety_state: mixed\n"
                "include_status: include_after_cleaning\n"
                "source_path: F:/bundle/guide.md\n"
                "---\n"
                "# Guide\n"
                "Use SSH from the console.\n",
                encoding="utf-8",
            )
            out = root / "cleaned"

            clean_docs.clean_documents(normalized, out)

            cleaned = (out / "guide.md").read_text(encoding="utf-8")
            self.assertNotIn("source_selection_product_area:", cleaned)

    def test_secret_redaction_does_not_cross_line_boundaries(self):
        cleaned, _ = clean_docs.clean_text("\u5bc6\u7801\uff1a\n\u4e0b\u4e00\u6bb5\u516c\u5f00\u8bf4\u660e\n")

        self.assertIn("\u4e0b\u4e00\u6bb5\u516c\u5f00\u8bf4\u660e", cleaned)

    def test_staff_name_sidecar_redacts_review_followup_name(self):
        cleaned, _ = clean_docs.clean_text("\u5ba2\u6237\u7ecf\u7406\uff1a\u6768\u601d\u6e90 siyuan.yang")

        self.assertNotIn("\u6768\u601d\u6e90", cleaned)

    def test_mine_internal_cases_redacts_and_validates_approvals(self):
        raw = "\n".join(
            [
                "2026-05-13 10:00 \u5ba2\u6237A: instance uhost-abc123 init failed, password=secret, link https://console.compshare.cn/workorder/1",
                "\u5f20\u6167 4-28 15:49 @\u5434\u5bb6\u6b22(jiahuan.wu) bsi-0ddf786952\u4e91\u76d8\u6269\u5bb9 _uhost_uhost-1oxs60xxkbeg \u5bc6\u7801\uff1acompshare123 \u5f3a\u54e5 \u8054\u7cfb\u8fd0\u8425(\u5f20\u6167/\u5f20\u96e8\u6b23), node_token: abc, \u975e\u6807\u6302\u76d8, SPT\u5de5\u5177, \u7f57\u76d8, @\u4f18\u4e91\u667a\u7b97QA\u5c0f\u52a9\u624b",
                "",
                "2026-05-13 11:00 \u5ba2\u6237B: how to buy resource package?",
                "2026-05-13 11:01 engineer (qi.li): guide user to console resource purchase page.",
            ]
        )

        cases = mine_internal_cases.mine_cases(raw, source_id="wxwork-spt-record-2026-05")

        self.assertEqual(len(cases), 2)
        for case in cases:
            for field in ("redacted_text", "issue_pattern", "resolution", "user_safe_answer_candidate"):
                value = case.get(field) or ""
                self.assertNotRegex(value, r"\([A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)+\)")
                self.assertNotIn("uhost-abc123", value)
                self.assertNotIn("bsi-0ddf786952", value)
                self.assertNotIn("uhost-1oxs60xxkbeg", value)
                self.assertNotIn("password=secret", value)
                self.assertNotIn("\u5bc6\u7801\uff1acompshare123", value)
                self.assertNotIn("workorder", value)
                self.assertNotIn("node_token", value)
                self.assertNotIn("\u5f20\u6167", value)
                self.assertNotIn("\u5f20\u96e8\u6b23", value)
                self.assertNotIn("\u5f3a\u54e5", value)
                self.assertNotIn("@", value)
                self.assertNotIn("SPT\u5de5\u5177", value)
        self.assertIn(cases[0]["label"], {"eval_only", "internal_only"})
        self.assertEqual(cases[1]["label"], "faq_candidate")

        templates = mine_internal_cases.approval_templates(cases)
        self.assertEqual(len(templates), 1)
        required = {
            "case_id",
            "source_hash",
            "redaction_status",
            "rewrite_path",
            "approved_by",
            "approved_at",
            "allowed_product_area",
            "blocked_phrases_checked",
            "final_runtime_chunk_id",
        }
        self.assertTrue(required.issubset(templates[0]))
        self.assertEqual(templates[0]["source_hash"], cases[1]["redacted_case_hash"])
        self.assertNotIn("redacted_text", templates[0])

        approvals = [mine_internal_cases.approval_record(cases[1], reviewer="reviewer", product_area="resource_purchase")]
        self.assertEqual(validate_case_approvals.validate_case_approvals(cases, approvals), {"approved_count": 1})

        broken = [dict(approvals[0], source_hash="bad")]
        with self.assertRaisesRegex(ValueError, "unknown source_hash"):
            validate_case_approvals.validate_case_approvals(cases, broken)

        missing = [dict(approvals[0])]
        del missing[0]["blocked_phrases_checked"]
        with self.assertRaisesRegex(ValueError, "missing blocked_phrases_checked"):
            validate_case_approvals.validate_case_approvals(cases, missing)

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
                "question_patterns": ["init failed"],
                "content": "Check console status first, then contact platform support if needed.",
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

    def test_validate_chunks_rejects_asset_refs_caption_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / "chunks.jsonl"
            chunk = {
                "chunk_id": "w0-login-doc-001",
                "kb_version": "kb.test",
                "source_type": "faq",
                "product_area": "login",
                "acl": "customer_safe",
                "title": "Login",
                "question_patterns": ["How to login"],
                "content": "Step one.\n[\u56fe\u8bf4] Login screenshot.",
                "source_refs": ["doc"],
                "asset_refs": ["asset-1", "asset-2"],
                "confidence": "high",
                "valid_from": "2026-05-13",
                "evidence_kind": "knowledge",
                "surface_url": None,
                "retrieval_score_hint": None,
            }
            path.write_text(json.dumps(chunk, ensure_ascii=False) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "asset_refs count 2 does not match caption count 1"):
                validate_chunks.validate_chunks(path)

    def test_validate_chunks_rejects_mid_asset_note_split(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "chunks.jsonl"
            chunk = self._valid_chunk(content="Step one.\n<!-- asset_note: {\"asset_id\": \"asset-1\"")
            path.write_text(json.dumps(chunk, ensure_ascii=False) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "asset_note"):
                validate_chunks.validate_chunks(path)

    def test_validate_chunks_rejects_oversize(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "chunks.jsonl"
            chunk = self._valid_chunk(content="A" * (validate_chunks.MAX_CHUNK_CONTENT_RUNES + 1))
            path.write_text(json.dumps(chunk, ensure_ascii=False) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "MAX_CHUNK_CONTENT_RUNES"):
                validate_chunks.validate_chunks(path)

    def test_validate_chunks_accepts_exactly_max(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "chunks.jsonl"
            chunk = self._valid_chunk(content="A" * validate_chunks.MAX_CHUNK_CONTENT_RUNES)
            path.write_text(json.dumps(chunk, ensure_ascii=False) + "\n", encoding="utf-8")

            self.assertEqual(validate_chunks.validate_chunks(path)["chunk_count"], 1)

    def test_parse_sections_normalizes_no_space_heading(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "# Windows audio\nStep one.\n##步骤二:\nContinue with audio settings.\n",
                name="windows-audio__guide.md",
            )
            out = root / "sections.jsonl"

            summary = parse_sections.parse_cleaned_docs(cleaned, out)

            rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual(summary["doc_count"], 1)
            self.assertEqual([row["heading_text"] for row in rows], ["Windows audio", "步骤二:"])
            self.assertIn("heading_normalized_no_space", rows[1]["risk_flags"])

    def test_parse_sections_rejects_truly_invalid_heading(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Good\nBody.\n####### Bad\nBroken.\n")

            with self.assertRaisesRegex(ValueError, "invalid heading"):
                parse_sections.parse_cleaned_docs(cleaned, root / "sections.jsonl")

    def test_parse_sections_treats_empty_heading_marker_as_content(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Guide\nBefore.\n#\nAfter.\n")

            parse_sections.parse_cleaned_docs(cleaned, root / "sections.jsonl")

            rows = [json.loads(line) for line in (root / "sections.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(len(rows), 1)
            self.assertIn("#", rows[0]["content"])
            self.assertIn("empty_heading_marker_as_content", rows[0]["risk_flags"])

    def test_chunk_plan_empty_classifier_falls_back_to_mixed_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# FAQ\nQuestion\nAnswer.\n")
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            calls = {"count": 0}

            def empty_classifier(payload):
                calls["count"] += 1
                return {}

            summary = chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=empty_classifier,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(calls["count"], 2)
            self.assertEqual(summary["mixed_needs_review"], 1)
            self.assertEqual(rows[0]["strategy"], "mixed_needs_review")
            self.assertEqual(rows[0]["chunk_plan_error"], "classifier_returned_empty")

    def test_chunk_plan_uses_bootstrap_plan_before_classifier(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# FAQ\nQuestion\nAnswer.\n", name="guide__faq.md")
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            bootstrap = root / "bootstrap.jsonl"
            bootstrap.write_text(
                json.dumps(
                    {
                        "source_doc_id": "guide",
                        "strategy": "single_topic_reference",
                        "rationale": "Opus bootstrap",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_index_range": [0, 0],
                                "title": "Guide FAQ",
                                "product_area": "login",
                                "question_patterns": ["How do I use the guide?"],
                            }
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            def should_not_call(_payload):
                raise AssertionError("classifier should not run for bootstrap-covered docs")

            summary = chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=should_not_call,
                bootstrap_plans_path=bootstrap,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(summary["chunk_count"], 1)
            self.assertEqual(rows[0]["strategy"], "single_topic_reference")
            self.assertEqual(rows[0]["chunks"][0]["question_patterns"], ["How do I use the guide?"])

    def test_chunk_plan_preserves_bootstrap_source_doc_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Billing\nPay monthly.\n", name="gitlab-compshare-docs__bill.md")
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            bootstrap = root / "bootstrap.jsonl"
            bootstrap.write_text(
                json.dumps(
                    {
                        "source_doc_id": "gitlab-compshare-docs__bill",
                        "strategy": "single_topic_reference",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_index_range": [0, 0],
                                "title": "Billing",
                                "product_area": "billing_rule",
                                "question_patterns": ["How is billing calculated?"],
                            }
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=lambda _payload: {},
                bootstrap_plans_path=bootstrap,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(rows[0]["source_doc_id"], "gitlab-compshare-docs__bill")

    def test_chunk_plan_derives_section_range_from_included_headings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "# Billing\nOverview.\n## Hourly\nPay hourly.\n## Monthly\nPay monthly.\n",
                name="billing__guide.md",
            )
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            bootstrap = root / "bootstrap.jsonl"
            bootstrap.write_text(
                json.dumps(
                    {
                        "source_doc_id": "billing",
                        "strategy": "single_topic_reference",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_headings_included": ["Billing", "Hourly", "Monthly"],
                                "title": "Billing complete guide",
                                "product_area": "billing_rule",
                                "question_patterns": ["How does billing work?"],
                            }
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            summary = chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=lambda _payload: {},
                bootstrap_plans_path=bootstrap,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(summary["chunk_count"], 1)
            self.assertEqual(rows[0]["chunks"][0]["section_index_range"], [0, 2])

    def test_chunk_plan_derives_repeated_headings_in_order(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "# Monitor\nIntro.\n## Linux\nLinux setup.\n### Notes\nLinux notes.\n## Windows\nWindows setup.\n### Notes\nWindows notes.\n",
                name="monitor__guide.md",
            )
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            bootstrap = root / "bootstrap.jsonl"
            bootstrap.write_text(
                json.dumps(
                    {
                        "source_doc_id": "monitor",
                        "strategy": "multi_topic_faq",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_headings_included": ["Linux", "Notes"],
                                "title": "Linux monitor",
                                "product_area": "monitor",
                                "question_patterns": ["Linux monitor setup"],
                            },
                            {
                                "chunk_index": 1,
                                "section_headings_included": ["Windows", "Notes"],
                                "title": "Windows monitor",
                                "product_area": "monitor",
                                "question_patterns": ["Windows monitor setup"],
                            },
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=lambda _payload: {},
                bootstrap_plans_path=bootstrap,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(rows[0]["chunks"][0]["section_index_range"], [1, 2])
            self.assertEqual(rows[0]["chunks"][1]["section_index_range"], [3, 4])

    def test_chunk_plan_expands_section_ranges_to_cover_nested_children(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "# Publish image\nIntro.\n## Step one\nDo one.\n### Detail A\nMore A.\n### Detail B\nMore B.\n## Step two\nDo two.\n",
                name="image__guide.md",
            )
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            bootstrap = root / "bootstrap.jsonl"
            bootstrap.write_text(
                json.dumps(
                    {
                        "source_doc_id": "image",
                        "strategy": "staged_long_procedure",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_headings_included": ["Publish image", "Step one"],
                                "title": "Publish image setup",
                                "product_area": "image",
                                "question_patterns": ["How do I start publishing an image?"],
                            },
                            {
                                "chunk_index": 1,
                                "section_headings_included": ["Step two"],
                                "title": "Publish image final step",
                                "product_area": "image",
                                "question_patterns": ["How do I finish publishing an image?"],
                            },
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            chunk_plan.plan_chunks(
                cleaned,
                sections,
                root / "chunk_plans.jsonl",
                classifier=lambda _payload: {},
                bootstrap_plans_path=bootstrap,
                model="fake-model",
            )

            rows = [json.loads(line) for line in (root / "chunk_plans.jsonl").read_text(encoding="utf-8").splitlines()]
            self.assertEqual(rows[0]["chunks"][0]["section_index_range"], [0, 3])
            self.assertEqual(rows[0]["chunks"][1]["section_index_range"], [4, 4])

    def test_chunk_docs_uses_chunk_plan_to_keep_single_topic_doc_intact(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "# Windows audio\nRun gpedit.\n##步骤二:\nEnable audio redirection.\n## Step three\nRestart audio service.\n",
                name="windows-audio__guide.md",
            )
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            plans = root / "chunk_plans.jsonl"
            plan = {
                "source_doc_id": "windows-audio",
                "source_ref": "windows-audio__guide",
                "strategy": "single_topic_procedure",
                "chunks": [
                    {
                        "chunk_index": 0,
                        "section_index_range": [0, 2],
                        "title": "Windows audio complete flow",
                        "product_area": "windows",
                        "question_patterns": ["Windows remote desktop has no audio"],
                    }
                ],
            }
            plans.write_text(json.dumps(plan, ensure_ascii=False) + "\n", encoding="utf-8")
            out = root / "chunks.jsonl"

            summary = chunk_docs.chunk_documents(
                cleaned,
                out,
                kb_version="kb.test",
                valid_from="2026-05-13",
                sections_path=sections,
                chunk_plans_path=plans,
                require_complete_inputs=False,
            )

            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual(summary["chunk_count"], 1)
            self.assertEqual(chunks[0]["title"], "Windows audio complete flow")
            self.assertEqual(chunks[0]["question_patterns"], ["Windows remote desktop has no audio"])
            self.assertEqual(chunks[0]["section_index_range"], [0, 2])

    def test_chunk_docs_anchor_boundaries_tolerate_quote_variants(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(
                root,
                "ssh无法连接\nSSH help.\n关于“无卡启动模式”的使用和限制\nNo-card help.\n创建的实例，在jupyterLab运行代码后\nJupyter help.\n",
                name="usage__faq.md",
            )
            sections = root / "sections.jsonl"
            parse_sections.parse_cleaned_docs(cleaned, sections)
            plans = root / "chunk_plans.jsonl"
            plans.write_text(
                json.dumps(
                    {
                        "source_doc_id": "usage",
                        "source_ref": "usage__faq",
                        "strategy": "multi_topic_faq",
                        "chunks": [
                            {
                                "chunk_index": 0,
                                "section_anchor_text_start": '关于"无卡启动模式"',
                                "section_anchor_text_end": "创建的实例，在jupyterLab",
                                "title": "No-card mode",
                                "product_area": "login",
                                "question_patterns": ["无卡启动怎么用"],
                            }
                        ],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )

            chunk_docs.chunk_documents(
                cleaned,
                root / "chunks.jsonl",
                kb_version="kb.test",
                valid_from="2026-05-13",
                sections_path=sections,
                chunk_plans_path=plans,
                require_complete_inputs=False,
            )

            chunk = json.loads((root / "chunks.jsonl").read_text(encoding="utf-8").strip())
            self.assertIn("关于“无卡启动模式”", chunk["content"])
            self.assertNotIn("创建的实例", chunk["content"])

    def test_validate_chunks_rejects_section_overlap_and_gap(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            sections_path = root / "sections.jsonl"
            section_rows = [
                {"source_ref": "guide", "source_doc_id": "guide", "section_index": index}
                for index in range(3)
            ]
            sections_path.write_text("".join(json.dumps(row) + "\n" for row in section_rows), encoding="utf-8")

            def chunk(chunk_id, section_range):
                return {
                    "chunk_id": chunk_id,
                    "kb_version": "kb.test",
                    "source_type": "faq",
                    "product_area": "login",
                    "acl": "customer_safe",
                    "title": chunk_id,
                    "content": "Use SSH.",
                    "source_refs": ["guide"],
                    "asset_refs": [],
                    "confidence": "high",
                    "valid_from": "2026-05-13",
                    "evidence_kind": "knowledge",
                    "surface_url": None,
                    "retrieval_score_hint": None,
                    "section_index_range": section_range,
                }

            chunks_path.write_text(
                json.dumps(chunk("w0-login-a", [0, 1]), ensure_ascii=False)
                + "\n"
                + json.dumps(chunk("w0-login-b", [1, 2]), ensure_ascii=False)
                + "\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "covered by multiple chunks"):
                validate_chunks.validate_chunks(chunks_path, sections_path=sections_path)

            chunks_path.write_text(
                json.dumps(chunk("w0-login-a", [0, 0]), ensure_ascii=False)
                + "\n"
                + json.dumps(chunk("w0-login-b", [2, 2]), ensure_ascii=False)
                + "\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "not covered"):
                validate_chunks.validate_chunks(chunks_path, sections_path=sections_path)

    def test_verify_chunk_plan_anchors_detects_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            plans = root / "plans.jsonl"
            pinned = root / "pinned.json"
            plans.write_text(
                json.dumps(
                    {
                        "source_doc_id": "guide",
                        "strategy": "single_topic_procedure",
                        "chunks": [{"product_area": "windows"}],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            pinned.write_text(
                json.dumps(
                    {
                        "anchors": [
                            {
                                "source_doc_id": "guide",
                                "expected_strategy": "multi_topic_faq",
                                "expected_chunk_count": 1,
                                "expected_product_areas": ["windows"],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "strategy mismatch"):
                verify_chunk_plan_anchors.verify_chunk_plan_anchors(plans, pinned)

    def test_verify_section_lists_detects_heading_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            sections = root / "sections.jsonl"
            pinned = root / "pinned_sections.json"
            sections.write_text(
                json.dumps(
                    {
                        "source_ref": "guide",
                        "source_doc_id": "guide",
                        "section_index": 0,
                        "heading_text": "Actual heading",
                        "heading_path": ["Actual heading"],
                        "risk_flags": [],
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            pinned.write_text(
                json.dumps(
                    {
                        "sources": [
                            {
                                "source_ref": "guide",
                                "expected_section_count": 1,
                                "sections": [
                                    {
                                        "section_index": 0,
                                        "heading_text": "Expected heading",
                                        "heading_path": ["Expected heading"],
                                        "risk_flags": [],
                                    }
                                ],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "heading mismatch"):
                verify_section_lists.verify_section_lists(sections, pinned)

    def test_chunk_docs_emits_valid_chunks_and_preserves_asset_refs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            doc = cleaned / "usage-faq.md"
            doc.write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# Login guidance\n"
                "Use SSH or Jupyter from the console. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":true,\"visual_type\":\"operation_screenshot\",\"description\":\"Console SSH entry\",\"user_action\":\"Click SSH\",\"next_step\":\"Connect\"} -->\n\n"
                "## Windows RDP\n"
                "Open Windows remote desktop from the console and use the instance public IP.\n",
                encoding="utf-8",
            )
            out = root / "chunks.jsonl"

            summary = chunk_docs.chunk_documents(cleaned, out, kb_version="kb.test", valid_from="2026-05-13", require_complete_inputs=False)

            self.assertGreaterEqual(summary["chunk_count"], 2)
            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertTrue(all(chunk["source_type"] in {"faq", "runbook"} for chunk in chunks))
            self.assertTrue(any("asset-1" in chunk["asset_refs"] for chunk in chunks))
            self.assertEqual(validate_chunks.validate_chunks(out)["chunk_count"], len(chunks))

    def test_chunk_docs_assigns_asset_refs_to_matching_split_part(self):
        note1 = json.dumps(
            {
                "asset_id": "asset-1",
                "include_in_rag": True,
                "visual_type": "operation_screenshot",
                "description": "First screenshot",
            },
            ensure_ascii=False,
        )
        note2 = json.dumps(
            {
                "asset_id": "asset-2",
                "include_in_rag": True,
                "visual_type": "operation_screenshot",
                "description": "Second screenshot",
            },
            ensure_ascii=False,
        )
        chunks = self._chunk_text(
            "# Login\n"
            f"First paragraph with enough text for chunking. <!-- asset_note: {note1} -->\n\n"
            f"Second paragraph with enough text for chunking. <!-- asset_note: {note2} -->\n",
            max_chars=90,
        )

        chunks_with_assets = [chunk for chunk in chunks if chunk["asset_refs"]]
        self.assertEqual([chunk["asset_refs"] for chunk in chunks_with_assets], [["asset-1"], ["asset-2"]])
        for chunk in chunks_with_assets:
            self.assertEqual(len(chunk["asset_refs"]), chunk["content"].count("[\u56fe\u8bf4]"))

    def test_chunk_docs_rejects_raw_markdown_image(self):
        with self.assertRaisesRegex(ValueError, "raw markdown image"):
            self._chunk_text(
                "# Image\n"
                "This paragraph keeps a raw image and must fail before promotion.\n"
                "![deploy](\n"
                "https://www-s.ucloud.cn/path/deploy.png)\n",
            )

    def test_chunk_docs_uses_front_matter_product_area_for_error_codes(self):
        chunks = self._chunk_cleaned_doc(
            [
                "source_trace_hash: abc",
                "safety_state: customer_safe_cleaned",
                "include_status: include_after_cleaning",
                "source_selection_product_area: init_failure",
            ],
            "# Error codes\n"
            "Error 226601 says the instance is in arrears and billing payment is required.\n"
            "Error 226604 says initialization failed because resources are not enough.\n",
            name="gitlab-compshare-docs__compshareerrorcode.md",
        )

        self.assertEqual({chunk["product_area"] for chunk in chunks}, {"init_failure"})

    def test_chunk_docs_uses_front_matter_product_area_for_resource_creation(self):
        chunks = self._chunk_cleaned_doc(
            [
                "source_trace_hash: abc",
                "safety_state: customer_safe_cleaned",
                "include_status: include_after_cleaning",
                "source_selection_product_area: resource_purchase",
            ],
            "# Create GPU instance\n"
            "Choose an image, GPU type, billing mode, and price option, then click deploy to purchase resources.\n",
            name="gitlab-compshare-docs__createresources.md",
        )

        self.assertEqual({chunk["product_area"] for chunk in chunks}, {"resource_purchase"})

    def test_chunk_docs_falls_back_when_front_matter_product_area_missing_empty_or_invalid(self):
        cases = [
            ([], "billing_rule"),
            (["source_selection_product_area:"], "billing_rule"),
            (["source_selection_product_area: not_a_real_area"], "billing_rule"),
        ]
        for extra_front_matter, expected_area in cases:
            with self.subTest(extra_front_matter=extra_front_matter):
                chunks = self._chunk_cleaned_doc(
                    [
                        "source_trace_hash: abc",
                        "safety_state: customer_safe_cleaned",
                        "include_status: include_after_cleaning",
                        *extra_front_matter,
                    ],
                    "# Billing fallback\nThis billing invoice refund guide should keep using keyword inference.\n",
                )

                self.assertEqual({chunk["product_area"] for chunk in chunks}, {expected_area})

    def test_chunk_id_changes_with_front_matter_product_area(self):
        body = "# Error codes\nInstance init failed after arrears were resolved and billing was paid.\n"
        without_area = self._chunk_cleaned_doc(
            [
                "source_trace_hash: abc",
                "safety_state: customer_safe_cleaned",
                "include_status: include_after_cleaning",
            ],
            body,
            name="gitlab-compshare-docs__compshareerrorcode.md",
        )[0]
        with_area = self._chunk_cleaned_doc(
            [
                "source_trace_hash: abc",
                "safety_state: customer_safe_cleaned",
                "include_status: include_after_cleaning",
                "source_selection_product_area: init_failure",
            ],
            body,
            name="gitlab-compshare-docs__compshareerrorcode.md",
        )[0]

        self.assertNotEqual(without_area["chunk_id"], with_area["chunk_id"])
        self.assertTrue(with_area["chunk_id"].startswith("w0-init_failure-"))

    def test_chunk_docs_uses_section_label_sidecar_for_multi_topic_faq(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "feishu-usage-faq-latest__feishu-usage-faq-latest.md").write_text(
                "\n".join(
                    [
                        "---",
                        "source_trace_hash: abc",
                        "safety_state: customer_safe_cleaned",
                        "include_status: include_after_cleaning",
                        "---",
                        "优云智算使用问题FAQ 副本",
                        "GPU实例常见问题解答",
                        "",
                        "实例相关 -- 卡初始化、启动失败、GPU使用等问题",
                        "实例卡初始化或卡启动中",
                        "",
                        "容器启动失败，或者容器环境在使用中被破坏，会导致实例卡初始化或卡启动中，处理请加群联系群主。",
                        "",
                        "计费相关 -- 计费模式、扣费标准、账单、发票等问题",
                        "2.4 关于账号存在欠费订单无法使用的情况说明",
                        "",
                        "每个账号下若存在欠费订单则无法使用和创建新资源，请先前往财务中心手动支付后方可使用。",
                    ]
                ),
                encoding="utf-8",
            )
            config = root / "multi_topic_sources.json"
            config.write_text(
                json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "feishu-usage-faq-latest"}]}),
                encoding="utf-8",
            )
            labels = root / "section_labels.jsonl"

            label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {
                    "selected_area": "init_failure" if "初始化" in target["content"] else "billing_rule",
                    "confidence": 0.96,
                    "reasoning": "test classifier",
                },
            )
            out = root / "chunks.jsonl"
            chunk_docs.chunk_documents(cleaned, out, section_labels_path=labels, require_complete_inputs=False)

            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            init_chunks = [chunk for chunk in chunks if chunk["product_area"] == "init_failure"]
            billing_chunks = [chunk for chunk in chunks if chunk["product_area"] == "billing_rule"]
            self.assertTrue(any("实例卡初始化或卡启动中" in chunk["title"] for chunk in init_chunks))
            self.assertTrue(any("欠费订单" in chunk["content"] for chunk in billing_chunks))
            self.assertFalse(any("欠费订单" in chunk["content"] for chunk in init_chunks))

    def test_stable_key_product_area_independent(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# FAQ\n同一段内容用于验证标签变化不会改变稳定键。\n",
                encoding="utf-8",
            )
            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "faq"}]}), encoding="utf-8")
            first = root / "first.jsonl"
            second = root / "second.jsonl"

            label_sections.label_sections(
                cleaned,
                config,
                first,
                classifier=lambda target: {"selected_area": "init_failure", "confidence": 0.95, "reasoning": "first"},
            )
            label_sections.label_sections(
                cleaned,
                config,
                second,
                classifier=lambda target: {"selected_area": "billing_rule", "confidence": 0.95, "reasoning": "second"},
            )

            first_row = json.loads(first.read_text(encoding="utf-8").splitlines()[0])
            second_row = json.loads(second.read_text(encoding="utf-8").splitlines()[0])
            self.assertEqual(first_row["key"], second_row["key"])
            self.assertNotEqual(first_row["selected_area"], second_row["selected_area"])

    def test_stable_key_invalidates_on_content_edit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            doc = cleaned / "faq.md"
            doc.write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# Billing\nThis billing invoice refund guide keeps keyword fallback when sidecar is stale.\n",
                encoding="utf-8",
            )
            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "faq"}]}), encoding="utf-8")
            labels = root / "labels.jsonl"
            label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {"selected_area": "init_failure", "confidence": 0.95, "reasoning": "old label"},
            )
            old_key = json.loads(labels.read_text(encoding="utf-8").splitlines()[0])["key"]

            doc.write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# Billing\nThis billing invoice refund guide keeps keyword fallback when sidecar is stale. Edited.\n",
                encoding="utf-8",
            )
            new_target = label_sections.iter_label_targets(cleaned, multi_topic_ids={"faq"})[0]
            self.assertNotEqual(old_key["content_sha256_prefix"], new_target["key"]["content_sha256_prefix"])

            out = root / "chunks.jsonl"
            chunk_docs.chunk_documents(cleaned, out, section_labels_path=labels, require_complete_inputs=False)
            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual({chunk["product_area"] for chunk in chunks}, {"billing_rule"})

    def test_audit_fields_complete(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# FAQ\nThis section cannot be assigned confidently.\n",
                encoding="utf-8",
            )
            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "faq"}]}), encoding="utf-8")
            labels = root / "labels.jsonl"
            label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {
                    "selected_area": "",
                    "empty_label_reason": "unclear",
                    "confidence": 0.20,
                    "reasoning": "not enough signal",
                },
                smoke_run_id="test-run",
            )
            row = json.loads(labels.read_text(encoding="utf-8").splitlines()[0])
            for field in ("model", "prompt_version", "labeled_at", "smoke_run_id", "confidence", "attempts"):
                self.assertIn(field, row)
            self.assertEqual(bool(row["selected_area"]) + bool(row["empty_label_reason"]), 1)
            self.assertEqual(row["empty_label_reason"], "unclear")
            self.assertEqual(row["smoke_run_id"], "test-run")

    def test_label_one_section_exception_fallback(self):
        target = {
            "key": {"source_doc_id": "faq", "section_index": 0, "content_sha256_prefix": "abc"},
            "source_ref": "faq",
            "section_title": "FAQ",
            "content": "A section that triggers a classifier exception.",
        }

        row = label_sections._label_one_section(
            target,
            classifier=lambda target: (_ for _ in ()).throw(RuntimeError("boom")),
            model="test-model",
            prompt_version="label_test",
            smoke_run_id="test-run",
        )

        self.assertEqual(row["selected_area"], "")
        self.assertEqual(row["empty_label_reason"], "classifier_error")
        self.assertEqual(row["status"], "needs_review")
        self.assertIn("RuntimeError", row["reasoning"])
        self.assertIn("boom", row["reasoning"])
        self.assertEqual(row["attempts"], 1)

    def test_empty_selection_retry_and_fallback(self):
        calls: list[dict] = []

        def classifier(target: dict) -> dict:
            calls.append(target)
            return {"selected_area": "", "empty_label_reason": "", "confidence": 0, "reasoning": ""}

        target = {
            "key": {"source_doc_id": "faq", "section_index": 0, "content_sha256_prefix": "abc"},
            "source_ref": "faq",
            "section_title": "FAQ",
            "content": "A section where the classifier returns an invalid empty label twice.",
        }

        row = label_sections._label_one_section(
            target,
            classifier=classifier,
            model="test-model",
            prompt_version="label_test",
            smoke_run_id="test-run",
        )

        self.assertEqual(len(calls), 2)
        self.assertEqual(row["selected_area"], "")
        self.assertEqual(row["empty_label_reason"], "classifier_returned_empty")
        self.assertEqual(row["attempts"], 2)
        self.assertEqual(row["status"], "needs_review")

    def test_verify_pinned_sections_acceptance_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            pinned = root / "pinned.json"
            labels = root / "labels.jsonl"
            needs_split = root / "needs_split.jsonl"
            chunks = root / "chunks.jsonl"
            pinned_rows = [
                {"source_doc_id": "faq", "section_title_substring": "Init", "expected_product_area": "init_failure"},
                {"source_doc_id": "faq", "section_title_substring": "Billing", "expected_product_area": "billing_rule"},
                {"source_doc_id": "faq", "section_title_substring": "Login", "expected_product_area": "login"},
                {"source_doc_id": "faq", "section_title_substring": "Image", "expected_product_area": "image"},
                {"source_doc_id": "faq", "section_title_substring": "Model", "expected_product_area": "modelverse"},
            ]
            label_rows = [
                {
                    "key": {"source_doc_id": "faq", "section_index": index, "content_sha256_prefix": f"hash{index}"},
                    "section_title": title,
                    "selected_area": area,
                    "empty_label_reason": "",
                    "confidence": 0.95,
                }
                for index, (title, area) in enumerate(
                    [
                        ("Init startup", "init_failure"),
                        ("Billing rules", "billing_rule"),
                        ("Login SSH", "login"),
                        ("Image guide", "image"),
                        ("Model package", "modelverse"),
                    ]
                )
            ]
            label_rows.append(
                {
                    "key": {"source_doc_id": "faq", "section_index": 99, "content_sha256_prefix": "empty"},
                    "section_title": "Unclear",
                    "selected_area": "",
                    "empty_label_reason": "unclear",
                    "confidence": 0,
                }
            )
            needs_split_rows = [
                {
                    "section_id": f"faq::{i}",
                    "source_doc_id": "faq",
                    "section_title": f"Mixed {i}",
                    "current_label_attempt": "",
                    "current_confidence": 0.5,
                    "preview_text": "This preview is intentionally long enough for the review queue contract.",
                    "flagged_at": "2026-05-14T00:00:00Z",
                }
                for i in range(6)
            ]
            areas = ["init_failure", "billing_rule", "login", "image", "modelverse", "windows"]
            chunk_rows = []
            for index in range(100):
                area = areas[index % len(areas)]
                chunk_rows.append(
                    {
                        "chunk_id": f"w0-{area}-{index}",
                        "product_area": area,
                    }
                )

            pinned.write_text(json.dumps(pinned_rows, ensure_ascii=False), encoding="utf-8")
            labels.write_text("\n".join(json.dumps(row, ensure_ascii=False) for row in label_rows) + "\n", encoding="utf-8")
            needs_split.write_text("\n".join(json.dumps(row, ensure_ascii=False) for row in needs_split_rows) + "\n", encoding="utf-8")
            chunks.write_text("\n".join(json.dumps(row, ensure_ascii=False) for row in chunk_rows) + "\n", encoding="utf-8")

            summary = verify_pinned_sections.verify_acceptance(
                pinned_path=pinned,
                labels_path=labels,
                needs_split_path=needs_split,
                chunks_path=chunks,
            )

            self.assertEqual(summary["pinned_count"], 5)
            self.assertTrue(summary["needs_split_warning"])
            self.assertEqual(summary["chunk_count"], 100)

            bad_rows = list(label_rows)
            bad_rows[-1] = dict(bad_rows[-1], empty_label_reason="")
            labels.write_text("\n".join(json.dumps(row, ensure_ascii=False) for row in bad_rows) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "empty_label_reason"):
                verify_pinned_sections.verify_acceptance(
                    pinned_path=pinned,
                    labels_path=labels,
                    needs_split_path=needs_split,
                    chunks_path=chunks,
                )

    def test_plain_faq_heading_detection_does_not_treat_answer_sentence_as_heading(self):
        sections = chunk_docs._split_sections(
            "\n".join(
                [
                    "实例相关 -- 卡初始化、启动失败、GPU使用等问题",
                    "实例卡初始化或卡启动中",
                    "",
                    "容器启动失败，或者容器环境在使用中被破坏，会导致实例卡初始化或卡启动中，处理请加群联系群主",
                    "",
                    "计费相关 -- 计费模式、扣费标准、账单、发票等问题",
                    "2.4 关于账号存在欠费订单无法使用的情况说明",
                    "",
                    "每个账号下若存在欠费订单则无法使用和创建新资源，请先前往财务中心手动支付后方可使用",
                ]
            ),
            fallback_title="FAQ",
        )

        by_title = {section["title"]: section["content"] for section in sections}
        self.assertIn("实例卡初始化或卡启动中", by_title)
        self.assertIn("容器启动失败", by_title["实例卡初始化或卡启动中"])
        self.assertIn("2.4 关于账号存在欠费订单无法使用的情况说明", by_title)
        self.assertIn("欠费订单", by_title["2.4 关于账号存在欠费订单无法使用的情况说明"])

    def test_label_sections_skips_sources_not_marked_multi_topic(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "gitlab-compshare-docs__compshareerrorcode.md").write_text(
                "---\n"
                "source_trace_hash: abc\n"
                "safety_state: customer_safe_cleaned\n"
                "include_status: include_after_cleaning\n"
                "source_selection_product_area: init_failure\n"
                "---\n"
                "# 常见错误码\n"
                "当前资源不足，请稍后再试。该错误码用于解释实例初始化或创建阶段的资源不足问题。\n",
                encoding="utf-8",
            )

            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": []}), encoding="utf-8")
            labels = root / "labels.jsonl"
            summary = label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {"selected_area": "billing_rule", "confidence": 0.99, "reasoning": "should not run"},
            )

            self.assertEqual(summary["labeled"], 0)
            self.assertEqual(labels.read_text(encoding="utf-8"), "")

    def test_chunk_docs_marks_low_confidence_section_label(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# Billing fallback\nThis billing invoice refund guide should keep using keyword inference.\n",
                encoding="utf-8",
            )
            labels = root / "labels.jsonl"
            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "faq"}]}), encoding="utf-8")
            label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {"selected_area": "init_failure", "confidence": 0.60, "reasoning": "too low"},
            )
            out = root / "chunks.jsonl"
            chunk_docs.chunk_documents(cleaned, out, section_labels_path=labels, require_complete_inputs=False)

            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual({chunk["product_area"] for chunk in chunks}, {"billing_rule"})

            label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {"selected_area": "init_failure", "confidence": 0.75, "reasoning": "review"},
            )
            chunk_docs.chunk_documents(cleaned, out, section_labels_path=labels, require_complete_inputs=False)
            chunks = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual({chunk["product_area"] for chunk in chunks}, {"init_failure"})
            self.assertTrue(all(chunk.get("low_confidence_label") is True for chunk in chunks))

    def test_label_sections_emits_review_queues_and_skips_needs_split_chunks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n"
                "# Mixed\nThis section combines billing rules and startup failures in one paragraph.\n",
                encoding="utf-8",
            )
            config = root / "multi_topic_sources.json"
            config.write_text(json.dumps({"schema_version": "multi_topic_sources.v1", "sources": [{"source_id": "faq"}]}), encoding="utf-8")
            labels = root / "section_labels.jsonl"
            summary = label_sections.label_sections(
                cleaned,
                config,
                labels,
                classifier=lambda target: {"needs_split": True, "selected_area": "billing_rule", "confidence": 0.90, "reasoning": "mixed"},
            )

            self.assertEqual(summary["needs_split"], 1)
            self.assertEqual(len((root / "needs_split.jsonl").read_text(encoding="utf-8").splitlines()), 1)
            self.assertEqual(len((root / "needs_review.jsonl").read_text(encoding="utf-8").splitlines()), 1)
            out = root / "chunks.jsonl"
            with self.assertRaisesRegex(ValueError, "chunks file is empty"):
                chunk_docs.chunk_documents(cleaned, out, section_labels_path=labels, require_complete_inputs=False)

    def test_asset_note_rendered_to_chunk_content(self):
        chunks = self._chunk_text(
            "# Login\n"
            "Use the console. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":true,\"visual_type\":\"operation_screenshot\",\"description\":\"X 按钮\",\"user_action\":\"点击\",\"next_step\":\"完成\"} -->\n"
        )

        joined = "\n".join(chunk["content"] for chunk in chunks)
        self.assertIn("[图说] X 按钮。操作：点击。下一步：完成。", joined)
        self.assertTrue(any("asset-1" in chunk["asset_refs"] for chunk in chunks))

    def test_asset_note_excluded_when_include_in_rag_false(self):
        chunks = self._chunk_text(
            "# Login\n"
            "Use the console to continue connecting from this workflow. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":false,\"visual_type\":\"operation_screenshot\",\"description\":\"hidden\"} -->\n"
        )

        joined = "\n".join(chunk["content"] for chunk in chunks)
        self.assertNotIn("[图说]", joined)
        self.assertFalse(any("asset-1" in chunk["asset_refs"] for chunk in chunks))

    def test_asset_note_skips_empty_fields(self):
        chunks = self._chunk_text(
            "# Login\n"
            "Use the console. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":true,\"visual_type\":\"operation_screenshot\",\"description\":\"A\",\"user_action\":\"\",\"next_step\":\"C\"} -->\n"
        )

        joined = "\n".join(chunk["content"] for chunk in chunks)
        self.assertIn("[图说] A。下一步：C。", joined)
        self.assertNotIn("操作：。", joined)

    def test_asset_note_with_highlighted_ui(self):
        chunks = self._chunk_text(
            "# Login\n"
            "Use the console. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":true,\"visual_type\":\"operation_screenshot\",\"description\":\"X\",\"highlighted_ui\":\"红框\",\"user_action\":\"点击\"} -->\n"
        )

        self.assertIn("重点：红框。", "\n".join(chunk["content"] for chunk in chunks))

    def test_asset_note_with_caveats(self):
        chunks = self._chunk_text(
            "# Login\n"
            "Use the console. <!-- asset_note: {\"asset_id\":\"asset-1\",\"include_in_rag\":true,\"visual_type\":\"operation_screenshot\",\"description\":\"X\",\"caveats\":\"不要复制密码\"} -->\n"
        )

        self.assertIn("注意：不要复制密码。", "\n".join(chunk["content"] for chunk in chunks))

    def test_asset_note_truncated_when_too_long(self):
        payload = {
            "asset_id": "asset-1",
            "include_in_rag": True,
            "visual_type": "operation_screenshot",
            "description": "A" * 500,
            "user_action": "点击",
        }

        rendered = chunk_docs._render_asset_note_text(payload)

        self.assertEqual(len(rendered), 283)
        self.assertTrue(rendered.endswith("..."))

    def test_chunk_split_preserves_numbered_list(self):
        numbered = "\n".join(f"{idx}. 执行第 {idx} 步并保持上下文" for idx in range(1, 12))
        chunks = self._chunk_text("# Procedure\n" + numbered + "\n", max_chars=80)

        joined = "\n\n".join(chunk["content"] for chunk in chunks)
        self.assertIn(numbered, joined)

    def test_chunk_split_fails_loud_when_oversize(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Long\n" + ("A" * 3000) + "\n")
            with self.assertRaisesRegex(ValueError, "add ## subheading"):
                chunk_docs.chunk_documents(cleaned, root / "chunks.jsonl", require_complete_inputs=False, max_chars=2000)

            oversized_steps = "\n".join(f"{idx}. " + ("步骤" * 500) for idx in range(1, 5))
            cleaned = self._write_cleaned_doc(root, "# Procedure\n" + oversized_steps + "\n", name="procedure.md")
            with self.assertRaisesRegex(ValueError, "MAX_CHUNK_CONTENT_RUNES"):
                chunk_docs.chunk_documents(cleaned, root / "chunks2.jsonl", require_complete_inputs=False, max_chars=2000)

    def test_chunk_docs_default_max_chars_is_2000(self):
        paragraph = "This sentence stays in one default-sized chunk. " * 20
        chunks = self._chunk_text("# Default size\n" + paragraph + "\n")

        self.assertEqual(len(chunks), 1)
        self.assertGreater(len(chunks[0]["content"]), 300)

    def test_chunk_docs_keeps_redacted_placeholders_with_medium_confidence(self):
        chunks = self._chunk_text("# Redacted\nUse account [PERSON_REDACTED] after [SECRET_REDACTED] has been removed.\n")

        self.assertEqual(len(chunks), 1)
        self.assertIn("[PERSON_REDACTED]", chunks[0]["content"])
        self.assertIn("[SECRET_REDACTED]", chunks[0]["content"])
        self.assertEqual(chunks[0]["confidence"], "medium")

    def test_chunk_docs_redacted_and_clean_sections_keep_separate_confidence(self):
        chunks = self._chunk_text(
            "# Clean\nUse SSH from the console after the instance is running.\n\n"
            "# Redacted\nUse [PRIVATE_PROCESS_REDACTED] only after the private detail was removed.\n"
        )
        by_title = {chunk["title"]: chunk for chunk in chunks}

        self.assertEqual(by_title["Clean"]["confidence"], "high")
        self.assertEqual(by_title["Redacted"]["confidence"], "medium")

    def test_chunk_docs_keeps_doc_with_only_redacted_placeholder_text(self):
        chunks = self._chunk_text("# Only redacted\n[RESOURCE_ID_REDACTED] has been removed before publishing this safe guidance.\n")

        self.assertEqual(len(chunks), 1)
        self.assertEqual(chunks[0]["confidence"], "medium")

    def test_is_procedural_block_requires_consecutive_and_dense_steps(self):
        dense = "\n".join(
            [
                "1. Open the console.",
                "2. Select the instance.",
                "Use the visible button.",
                "Confirm the action.",
                "Return to the list.",
            ]
        )
        sparse = "\n".join(
            [
                "This FAQ explains a long scenario.",
                "1. Open the console.",
                "2. Select the instance.",
                "The rest is background.",
                "More context.",
                "More context.",
                "More context.",
                "More context.",
                "More context.",
                "More context.",
            ]
        )
        separated = "\n".join(
            [
                "1. Open the console.",
                "Background text.",
                "2. Select the instance.",
                "More text.",
                "3. Confirm the action.",
            ]
        )

        self.assertTrue(chunk_docs._is_procedural_block(dense))
        self.assertFalse(chunk_docs._is_procedural_block(sparse))
        self.assertFalse(chunk_docs._is_procedural_block(separated))
        self.assertFalse(chunk_docs._is_procedural_block(""))

    def test_is_procedural_block_accepts_exact_density_boundary(self):
        paragraph = "\n".join(
            [
                "1. Open the console.",
                "2. Select the instance.",
                "Background text.",
                "More background.",
                "Final note.",
            ]
        )

        self.assertTrue(chunk_docs._is_procedural_block(paragraph))

    def test_chunk_docs_splits_long_faq_with_sparse_numbered_lines(self):
        lines = [f"FAQ background line {idx} explains the scenario in ordinary prose." for idx in range(1, 70)]
        lines.insert(10, "1. This numbered line is only an example in a long FAQ.")
        lines.insert(11, "2. This second example line should not make the whole FAQ procedural.")

        chunks = self._chunk_text("# Sparse FAQ\n" + "\n".join(lines) + "\n", max_chars=500)

        self.assertGreater(len(chunks), 1)

    def test_chunk_docs_keeps_long_dense_procedure_intact_within_loader_cap(self):
        lines = [f"{idx}. Complete action {idx} and keep this procedure together." for idx in range(1, 35)]

        chunks = self._chunk_text("# Procedure\n" + "\n".join(lines) + "\n", max_chars=500)

        self.assertEqual(len(chunks), 1)
        self.assertIn("34. Complete action 34", chunks[0]["content"])

    def test_render_asset_note_hard_fails_on_bad_json_in_production(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, '# Bad\nThis paragraph remains long enough for dry-run chunk validation. <!-- asset_note: {"asset_id":"asset-1", bad} -->\n')
            asset_notes = root / "asset_notes.jsonl"
            asset_notes.write_text(
                json.dumps(
                    {
                        "asset_id": "asset-1",
                        "include_in_rag": True,
                        "final_state": "included_with_vl_note",
                        "requires_review": False,
                        "description": "VL description",
                        "model_metadata": {"vl_executed": True},
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            links = root / "links.json"
            links.write_text(json.dumps({"links": []}, ensure_ascii=False), encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "invalid asset_note JSON"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

    def test_render_asset_note_soft_warns_on_bad_json_in_dry_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, '# Bad\nThis paragraph remains long enough for dry-run chunk validation. <!-- asset_note: {"asset_id":"asset-1", bad} -->\n')

            with self.assertLogs(chunk_docs.LOGGER, level="WARNING") as logs:
                summary = chunk_docs.chunk_documents(cleaned, root / "chunks.jsonl", require_complete_inputs=False)

            self.assertEqual(summary["chunk_count"], 1)
            self.assertIn("invalid asset_note JSON", "\n".join(logs.output))

    def test_model_smoke_emit_asset_notes_schema(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            out = root / "asset_notes.jsonl"
            results = [
                {
                    "sample_id": "visual-console_state",
                    "asset_id": "asset-1",
                    "visual_type": "console_state",
                    "asset": {
                        "asset_id": "asset-1",
                        "source_doc_id": "doc-1",
                        "heading_path": ["Login"],
                        "image_path": "step.png",
                        "nearby_text": "Click connect",
                    },
                    "vl_response": {
                        "description": "RDP dialog",
                        "highlighted_ui": "计算机输入框",
                        "user_action": "填写公网 IP",
                        "expected_input": "公网 IP",
                        "next_step": "点击连接",
                        "uncertainty": "仅描述可见控件",
                    },
                    "pass": True,
                }
            ]

            model_smoke._write_vl_asset_notes(out, results, model="qwen3-vl-flash", prompt_version="smoke_v1", smoke_run_id="run-1")

            rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual(len(rows), 1)
            row = rows[0]
            self.assertEqual(row["asset_id"], "asset-1")
            self.assertEqual(row["final_state"], "included_with_vl_note")
            self.assertIs(row["include_in_rag"], True)
            self.assertIs(row["model_metadata"]["vl_executed"], True)
            self.assertEqual(row["caveats"], "仅描述可见控件")

    def test_model_smoke_full_batch_selects_only_customer_facing_targets(self):
        manifest = {
            "assets": [
                {
                    "asset_id": "asset-console",
                    "source_id": "public-guide",
                    "image_path": "console.png",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "heading_path": ["Windows"],
                    "nearby_text": "远程桌面登录",
                },
                {
                    "asset_id": "asset-driver",
                    "sha256": "same-image",
                    "source_id": "public-guide",
                    "image_path": "driver.png",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "heading_path": ["NVIDIA 驱动安装"],
                    "nearby_text": "点击驱动下载",
                },
                {
                    "asset_id": "asset-driver-repeat",
                    "source_id": "public-guide",
                    "image_path": "driver-repeat.png",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "heading_path": ["NVIDIA driver install"],
                    "nearby_text": "Click driver download",
                    "sha256": "same-image",
                },
                {
                    "asset_id": "asset-public-unknown",
                    "source_id": "public-guide",
                    "image_path": "unknown-public.png",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "heading_path": ["FAQ screenshot"],
                    "nearby_text": "",
                },
                {
                    "asset_id": "asset-internal-unknown",
                    "source_id": "internal-guide",
                    "image_path": "unknown.png",
                    "final_state": "included_with_ocr_note",
                    "include_in_rag": True,
                    "heading_path": ["内部 case"],
                    "nearby_text": "",
                },
                {
                    "asset_id": "asset-logo",
                    "source_id": "public-guide",
                    "image_path": "logo.png",
                    "final_state": "excluded_low_value",
                    "include_in_rag": False,
                    "heading_path": [],
                    "nearby_text": "",
                    "exclusion_reason": "low_value",
                },
            ]
        }
        source_manifest = {
            "sources": [
                {"id": "public-guide", "include_status": "include_after_cleaning", "customer_safe": True, "type": "feishu_docx_raw"},
                {"id": "internal-guide", "include_status": "internal_reference_only_needs_customer_safe_split", "customer_safe": False, "type": "internal_case_chat_export"},
            ]
        }

        selected = model_smoke._select_full_vl_assets(manifest, source_manifest=source_manifest)

        self.assertEqual([item["asset"]["asset_id"] for item in selected], ["asset-console", "asset-driver", "asset-driver-repeat", "asset-public-unknown"])
        self.assertEqual({item["visual_type"] for item in selected}, {"console_state", "operation_screenshot", "unknown"})

    def test_model_smoke_asset_note_writer_skips_failed_vl_results(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            out = root / "asset_notes.jsonl"
            results = [
                {
                    "sample_id": "ok",
                    "asset": {"asset_id": "asset-ok", "image_path": "ok.png"},
                    "visual_type": "console_state",
                    "vl_response": {"description": "OK", "user_action": "Click", "no_hallucination": True},
                    "pass": True,
                },
                {
                    "sample_id": "bad",
                    "asset": {"asset_id": "asset-bad", "image_path": "bad.png"},
                    "visual_type": "console_state",
                    "vl_response": {"description": "Bad"},
                    "pass": False,
                },
            ]

            model_smoke._write_vl_asset_notes(out, results, model="qwen3-vl-flash", prompt_version="smoke_v1", smoke_run_id="run-1")

            rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual([row["asset_id"] for row in rows], ["asset-ok"])

    def test_model_smoke_asset_note_writer_keeps_core_valid_keyword_miss(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            out = root / "asset_notes.jsonl"
            results = [
                {
                    "sample_id": "keyword-miss",
                    "asset": {"asset_id": "asset-ok", "image_path": "ok.png"},
                    "visual_type": "console_state",
                    "vl_response": {
                        "description": "Valid UI description",
                        "highlighted_ui": "primary button",
                        "user_action": "click the primary button",
                        "no_hallucination": True,
                    },
                    "checks": {
                        "json": True,
                        "user_action": True,
                        "highlighted_ui": True,
                        "expected_keyword": False,
                        "no_hallucination": True,
                    },
                    "pass": False,
                }
            ]

            model_smoke._write_vl_asset_notes(out, results, model="qwen3-vl-flash", prompt_version="smoke_v1", smoke_run_id="run-1")

            rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual([row["asset_id"] for row in rows], ["asset-ok"])
            self.assertEqual(rows[0]["confidence"], "medium")

    def test_model_smoke_full_vl_summary_treats_core_valid_keyword_miss_as_usable(self):
        results = [
            {
                "sample_id": "keyword-miss",
                "checks": {
                    "json": True,
                    "user_action": True,
                    "highlighted_ui": True,
                    "expected_keyword": False,
                    "no_hallucination": True,
                },
                "pass": False,
            }
        ]

        summary = model_smoke._summarize_full_vl(results)

        self.assertTrue(summary["pass"])
        self.assertEqual(summary["usable"], 1)
        self.assertEqual(summary["failed_ids"], [])
        self.assertEqual(summary["keyword_failed_ids"], ["keyword-miss"])

    def test_model_smoke_asset_note_count_uses_usable_results(self):
        results = [
            {"pass": True, "checks": {"json": True, "user_action": True, "highlighted_ui": True, "no_hallucination": True}},
            {
                "pass": False,
                "checks": {
                    "json": True,
                    "user_action": True,
                    "highlighted_ui": True,
                    "expected_keyword": False,
                    "no_hallucination": True,
                },
            },
            {"pass": False, "checks": {"json": False, "user_action": False}},
        ]

        self.assertEqual(model_smoke._vl_asset_note_count(results), 2)

    def test_model_smoke_records_vl_errors_and_continues_batch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            image_a = root / "a.png"
            image_b = root / "b.png"
            image_a.write_bytes(b"\x89PNG\r\n\x1a\na")
            image_b.write_bytes(b"\x89PNG\r\n\x1a\nb")
            selected = [
                {
                    "sample_id": "first",
                    "asset": {"asset_id": "a", "image_path": str(image_a), "heading_path": []},
                    "visual_type": "console_state",
                    "expected_keywords": ["ok"],
                },
                {
                    "sample_id": "second",
                    "asset": {"asset_id": "b", "image_path": str(image_b), "heading_path": []},
                    "visual_type": "console_state",
                    "expected_keywords": ["ok"],
                },
            ]

            class FakeClient:
                def __init__(self):
                    self.calls = 0

                def chat(self, **kwargs):
                    self.calls += 1
                    if self.calls == 1:
                        raise TimeoutError("slow model")
                    return json.dumps(
                        {
                            "visible_text": ["ok"],
                            "description": "ok",
                            "highlighted_ui": "ok button",
                            "user_action": "click ok",
                            "expected_input": "",
                            "next_step": "continue",
                            "no_hallucination": True,
                        }
                    )

            results = model_smoke._run_vl_samples(FakeClient(), "qwen-test", selected)

            self.assertEqual(len(results), 2)
            self.assertFalse(results[0]["pass"])
            self.assertEqual(results[0]["error"], "slow model")
            self.assertTrue(results[1]["pass"])

    def test_chunk_docs_requires_case_approval_before_case_chunks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n# Billing\nShutdown billing follows the billing mode.\n",
                encoding="utf-8",
            )
            cases_path = root / "cases.jsonl"
            case = {
                "case_id": "wxwork-spt-record-2026-05:case-0001",
                "label": "faq_candidate",
                "issue_pattern": "How do I buy a resource package?",
                "resolution": "Guide the user to the console resource package page.",
                "user_safe_answer_candidate": "Open the console resource purchase page and choose the package.",
                "redacted_case_hash": "hash-1",
            }
            cases_path.write_text(json.dumps(case, ensure_ascii=False) + "\n", encoding="utf-8")
            no_approval_out = root / "chunks-no-approval.jsonl"

            no_approval_summary = chunk_docs.chunk_documents(
                cleaned,
                no_approval_out,
                kb_version="kb.test",
                valid_from="2026-05-13",
                cases_path=cases_path,
                require_complete_inputs=False,
            )
            no_approval_chunks = [json.loads(line) for line in no_approval_out.read_text(encoding="utf-8").splitlines()]
            self.assertEqual(no_approval_summary["case_chunk_count"], 0)
            self.assertFalse(any(ref.startswith("wxwork-spt-record-") for chunk in no_approval_chunks for ref in chunk["source_refs"]))

            approval = mine_internal_cases.approval_record(case, reviewer="reviewer", product_area="resource_purchase")
            approvals_path = root / "approvals.jsonl"
            approvals_path.write_text(json.dumps(approval, ensure_ascii=False) + "\n", encoding="utf-8")
            approved_out = root / "chunks-approved.jsonl"

            approved_summary = chunk_docs.chunk_documents(
                cleaned,
                approved_out,
                kb_version="kb.test",
                valid_from="2026-05-13",
                cases_path=cases_path,
                approvals_path=approvals_path,
                require_complete_inputs=False,
            )

            approved_chunks = [json.loads(line) for line in approved_out.read_text(encoding="utf-8").splitlines()]
            case_chunks = [chunk for chunk in approved_chunks if chunk["source_refs"] == [case["case_id"]]]
            self.assertEqual(approved_summary["case_chunk_count"], 1)
            self.assertEqual(len(case_chunks), 1)
            self.assertIn("approval_record_hash", case_chunks[0])
            self.assertEqual(validate_chunks.validate_chunks(approved_out)["chunk_count"], len(approved_chunks))

    def test_chunk_docs_dry_run_refuses_deploy_targets(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n# FAQ\nUse SSH from the console.\n",
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "dry-run chunking"):
                chunk_docs.chunk_documents(cleaned, root / "deploy" / "kb" / "stage2b_w0.jsonl", require_complete_inputs=False)

            with self.assertRaisesRegex(ValueError, "dry-run chunking"):
                chunk_docs.chunk_documents(cleaned, root / "chunks" / "stage2b_w0.jsonl", require_complete_inputs=False)

    def test_chunk_docs_refuses_incomplete_asset_and_link_processing(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            (cleaned / "faq.md").write_text(
                "---\nsource_trace_hash: abc\nsafety_state: customer_safe_cleaned\ninclude_status: include_after_cleaning\n---\n# Login\nUse SSH from the console.\n",
                encoding="utf-8",
            )
            asset_notes = root / "asset_notes.jsonl"
            asset_notes.write_text(
                json.dumps(
                    {
                        "asset_id": "asset-1",
                        "include_in_rag": True,
                        "final_state": "included_with_ocr_note",
                        "requires_review": True,
                        "description": "nearby text only",
                        "model_metadata": {"vl_executed": False},
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            links = root / "link_manifest.json"
            links.write_text(json.dumps({"links": []}, ensure_ascii=False), encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "not VL-ready"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

            asset_notes.write_text(
                json.dumps(
                    {
                        "asset_id": "asset-1",
                        "include_in_rag": True,
                        "final_state": "included_with_vl_note",
                        "requires_review": False,
                        "description": "The image shows the console SSH entry.",
                        "model_metadata": {"vl_executed": True},
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            links.write_text(json.dumps({"links": [{"link_id": "link-1", "final_state": "review_required"}]}, ensure_ascii=False), encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "unresolved before chunking"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

            links.write_text(json.dumps({"links": [{"link_id": "link-1", "final_state": "snapshotted"}]}, ensure_ascii=False), encoding="utf-8")
            summary = chunk_docs.chunk_documents(
                cleaned,
                root / "chunks.jsonl",
                asset_notes_path=asset_notes,
                link_manifest_path=links,
                require_complete_inputs=True,
            )

            self.assertEqual(summary["chunk_count"], 1)

    def test_chunk_docs_checks_unresolved_links_only_for_cleaned_sources(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Login\nUse SSH from the console.\n", name="included-source__guide.md")
            asset_notes = root / "asset_notes.jsonl"
            asset_notes.write_text("", encoding="utf-8")
            links = root / "link_manifest.json"
            links.write_text(
                json.dumps(
                    {
                        "links": [
                            {"link_id": "skipped-1", "source_id": "skipped-source", "final_state": "review_required"},
                            {"link_id": "included-1", "source_id": "included-source", "final_state": "snapshotted"},
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = chunk_docs.chunk_documents(
                cleaned,
                root / "chunks.jsonl",
                asset_notes_path=asset_notes,
                link_manifest_path=links,
                require_complete_inputs=True,
            )

            self.assertEqual(summary["chunk_count"], 1)

            links.write_text(
                json.dumps(
                    {"links": [{"link_id": "included-2", "source_id": "included-source", "final_state": "unknown"}]},
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "included-2: unresolved before chunking"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks2.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

    def test_chunk_docs_blocks_unresolved_links_without_source_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = self._write_cleaned_doc(root, "# Login\nUse SSH from the console.\n")
            asset_notes = root / "asset_notes.jsonl"
            asset_notes.write_text("", encoding="utf-8")
            links = root / "link_manifest.json"
            links.write_text(
                json.dumps({"links": [{"link_id": "link-1", "final_state": "review_required"}]}, ensure_ascii=False),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "link-1: unresolved before chunking"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

    def test_chunk_docs_blocks_unresolved_links_when_cleaned_source_set_is_empty(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cleaned = root / "cleaned"
            cleaned.mkdir()
            asset_notes = root / "asset_notes.jsonl"
            asset_notes.write_text("", encoding="utf-8")
            links = root / "link_manifest.json"
            links.write_text(
                json.dumps(
                    {"links": [{"link_id": "link-1", "source_id": "skipped-source", "final_state": "unknown"}]},
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "link-1: unresolved before chunking"):
                chunk_docs.chunk_documents(
                    cleaned,
                    root / "chunks.jsonl",
                    asset_notes_path=asset_notes,
                    link_manifest_path=links,
                    require_complete_inputs=True,
                )

    def test_generate_eval_questions_emits_fixed_behavior_mix(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            chunks = [
                {
                    "chunk_id": "w0-login-001",
                    "kb_version": "kb.test",
                    "source_type": "faq",
                    "product_area": "login",
                    "acl": "customer_safe",
                    "title": "Login guidance",
                    "question_patterns": ["How do I login?"],
                    "content": "Use SSH or Jupyter from the console.",
                    "source_refs": ["usage-faq"],
                    "asset_refs": [],
                    "confidence": "high",
                    "valid_from": "2026-05-13",
                    "evidence_kind": "knowledge",
                    "surface_url": None,
                    "retrieval_score_hint": None,
                },
                {
                    "chunk_id": "w0-billing-001",
                    "kb_version": "kb.test",
                    "source_type": "faq",
                    "product_area": "billing_rule",
                    "acl": "customer_safe",
                    "title": "Shutdown billing",
                    "question_patterns": ["Will shutdown still bill?"],
                    "content": "Shutdown billing follows the billing mode.",
                    "source_refs": ["billing-faq"],
                    "asset_refs": [],
                    "confidence": "high",
                    "valid_from": "2026-05-13",
                    "evidence_kind": "knowledge",
                    "surface_url": None,
                    "retrieval_score_hint": None,
                },
            ]
            chunks_path.write_text("".join(json.dumps(chunk, ensure_ascii=False) + "\n" for chunk in chunks), encoding="utf-8")
            cases_path = root / "cases.jsonl"
            cases_path.write_text(
                json.dumps(
                    {
                        "case_id": "wxwork-spt-record-2026-05:case-0001",
                        "label": "eval_only",
                        "issue_pattern": "[PERSON_REDACTED] 3-1 17:06",
                        "redacted_text": "资源 id: [RESOURCE_ID_REDACTED] 客户问题: [图片] 初始化失败",
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            out = root / "golden_questions.jsonl"

            def paraphraser(chunk: dict, retry: bool = False) -> list[str]:
                if chunk["chunk_id"] == "w0-login-001":
                    return [
                        "How can I connect to the instance from the console?",
                        "What should I use for SSH or Jupyter access?",
                    ]
                return [
                    "Does a stopped instance keep charging?",
                    "How is shutdown billing handled?",
                ]

            summary = generate_eval_questions.generate_eval_questions(
                chunks_path,
                out,
                min_questions=50,
                cases_path=cases_path,
                paraphraser=paraphraser,
            )

            questions = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            behaviors = {item["expected_behavior"] for item in questions}
            groups = {item["group"] for item in questions}
            self.assertGreaterEqual(summary["question_count"], 50)
            self.assertTrue({"answer", "refuse", "hard_block", "escalate"}.issubset(behaviors))
            self.assertTrue(all(item["expected_behavior"] in {"answer", "refuse", "hard_block", "escalate"} for item in questions))
            self.assertTrue(all(item["group"] in generate_eval_questions.EXPECTED_GROUPS for item in questions))
            self.assertTrue(generate_eval_questions.EXPECTED_GROUPS.issubset(groups))
            self.assertTrue(any(item["expected_chunk_ids"] == ["w0-login-001"] for item in questions))
            answer_questions = [item for item in questions if item["expected_behavior"] == "answer"]
            self.assertTrue(any(item["question"] == "How can I connect to the instance from the console?" for item in answer_questions))
            self.assertFalse(any(item["question"] == "How do I login?" for item in answer_questions))
            self.assertTrue(any(str(item.get("paraphrase_source") or "").endswith("eval_paraphrase_v1") for item in answer_questions))
            mined = [item for item in questions if item["source_refs"] == ["wxwork-spt-record-2026-05:case-0001"]]
            self.assertEqual(len(mined), 1)
            self.assertEqual(mined[0]["question"], "实例初始化失败时应该怎么处理？")
            self.assertNotIn("PERSON_REDACTED", mined[0]["question"])

    def test_generate_eval_questions_retries_byte_equal_paraphrases(self):
        chunk = {
            "chunk_id": "w0-login-001",
            "title": "Login guidance",
            "question_patterns": ["How do I login?"],
        }
        calls: list[bool] = []

        def paraphraser(_chunk: dict, retry: bool = False) -> list[str]:
            calls.append(retry)
            if retry:
                return ["How can I connect from the console?"]
            return ["How do I login?"]

        questions, log_record = generate_eval_questions._answer_questions_for_chunk(chunk, paraphraser=paraphraser)

        self.assertEqual(calls, [False, True])
        self.assertEqual(questions, ["How can I connect from the console?"])
        self.assertEqual(log_record["final_accepted"], ["How can I connect from the console?"])

    def test_generate_eval_questions_rejects_repeated_byte_equal_paraphrases(self):
        chunk = {
            "chunk_id": "w0-login-001",
            "title": "Login guidance",
            "question_patterns": ["How do I login?"],
        }

        def paraphraser(_chunk: dict, retry: bool = False) -> list[str]:
            return ["How do I login?", "Login guidance"]

        questions, log_record = generate_eval_questions._answer_questions_for_chunk(chunk, paraphraser=paraphraser)

        self.assertEqual(questions, [])
        self.assertEqual(len(log_record["paraphrases_generated"]), 2)
        self.assertEqual(log_record["final_accepted"], [])

    def test_verify_eval_questions_rejects_tautological_questions(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-ssh-a1b2c3d4",
                product_area="login",
                title="SSH login",
                content="Use SSH from the console.",
                question_patterns=["How do I use SSH?"],
            )
            self._write_jsonl(chunks_path, [chunk])
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I use SSH?",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                    },
                    {
                        "question_id": "q2",
                        "question": "SSH login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                    },
                ],
            )

            with self.assertRaisesRegex(ValueError, "PILLAR 0 FAIL"):
                verify_eval_questions.verify_eval_questions(questions_path, chunks_path)

    def test_verify_eval_questions_accepts_natural_paraphrase(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-ssh-a1b2c3d4",
                product_area="login",
                title="SSH login",
                content="Use SSH from the console.",
                question_patterns=["How do I use SSH?"],
            )
            self._write_jsonl(chunks_path, [chunk])
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "What should I do to connect from my IDE?",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                    }
                ],
            )

            summary = verify_eval_questions.verify_eval_questions(questions_path, chunks_path)

            self.assertEqual(summary["violations"], 0)

    def test_check_internal_leakage_flags_staff_name_and_internal_case(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            staff_name = next(iter(check_internal_leakage._load_staff_names()))
            clean_chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-clean-a1b2c3d4",
                product_area="login",
                title="Clean login",
                content="Use the console to connect.",
                question_patterns=["How do I connect?"],
            )
            staff_chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-staff-a1b2c3d4",
                product_area="login",
                title="Staff leak",
                content=f"Contact {staff_name} for this issue.",
                question_patterns=["Who handles this?"],
            )
            internal_case_chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-spt-a1b2c3d4",
                product_area="login",
                title="SPT leak",
                content="Customer-safe looking text.",
                question_patterns=["How do I connect?"],
            )
            internal_case_chunk["source_refs"] = ["wxwork-spt-record-2026-05:case-1"]
            self._write_jsonl(chunks_path, [clean_chunk, staff_chunk, internal_case_chunk])

            summary = check_internal_leakage.check_internal_leakage(chunks_path)

            self.assertEqual(summary["chunk_count"], 3)
            self.assertEqual(summary["flagged_count"], 2)
            findings = [finding for row in summary["flagged"] for finding in row["findings"]]
            self.assertTrue(any(finding.startswith("staff_name:") for finding in findings))
            self.assertTrue(any(finding.startswith("internal_case:") for finding in findings))

    def test_check_internal_leakage_cli_report_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-spt-a1b2c3d4",
                product_area="login",
                title="SPT leak",
                content="spt-12345 should never appear in deployed chunks.",
                question_patterns=["How do I connect?"],
            )
            self._write_jsonl(chunks_path, [chunk])

            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(check_internal_leakage.main(["--chunks", str(chunks_path), "--report-only"]), 0)
                self.assertEqual(check_internal_leakage.main(["--chunks", str(chunks_path)]), 1)

    def test_retrieval_scoring_tokenizes_nfkc_and_multiset_ngrams(self):
        tokens = retrieval_scoring.tokenize_text("  ＡＢＣ！ 远程桌面没声音？远程桌面  ")

        self.assertIn("abc", tokens)
        self.assertIn("远程", tokens)
        self.assertIn("远程桌", tokens)
        self.assertNotIn("！", tokens)
        self.assertEqual(tokens.count("远程"), 2)

    def test_retrieval_scoring_treats_cjk_extensions_like_runtime(self):
        ext_a = chr(0x3402)
        ext_b = chr(0x2000B)
        tokens = retrieval_scoring.tokenize_text(f"{ext_a}{ext_b}开机")

        self.assertIn(f"{ext_a}{ext_b}", tokens)
        self.assertIn(f"{ext_b}开", tokens)
        self.assertNotIn("é", retrieval_scoring.tokenize_text("é abc"))
        self.assertNotIn("か", retrieval_scoring.tokenize_text("かな"))
        self.assertNotIn("한", retrieval_scoring.tokenize_text("한글"))

    def test_evaluate_retrieval_matches_natural_chinese_paraphrase(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            out_path = root / "retrieval_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-windows-audio-a1b2c3d4",
                        product_area="windows",
                        title="远程 Windows 开启声音",
                        content="通过组策略启用远程桌面音频重定向，然后将 Windows Audio 服务设为自动并重启。",
                        question_patterns=["Windows 远程桌面没声音怎么办"],
                    ),
                    self._retrieval_eval_chunk(
                        chunk_id="w0-driver_cuda-install-a1b2c3d4",
                        product_area="driver_cuda",
                        title="CUDA 安装",
                        content="先安装 NVIDIA 驱动，再安装 CUDA Toolkit。",
                        question_patterns=["怎么安装 NVIDIA 驱动"],
                    ),
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "我远程连上 Windows 云服务器后没有声音",
                        "group": "windows_rdp_sound",
                        "product_area": "windows",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-windows-audio-a1b2c3d4"],
                        "source_refs": ["windows-audio"],
                    }
                ],
            )

            summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, out_path)

            self.assertEqual(summary["top_3_hit_rate"], 1.0)
            self.assertEqual(summary["trace_records"][0]["hit_items"][0]["chunk_id"], "w0-windows-audio-a1b2c3d4")

    def test_retriever_go_python_parity_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            go_out_path = root / "go_top3.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-windows-audio-a1b2c3d4",
                        product_area="windows",
                        title="远程 Windows 开启声音",
                        content="通过组策略启用远程桌面音频重定向，然后将 Windows Audio 服务设为自动并重启。",
                        question_patterns=["Windows 远程桌面没声音怎么办"],
                    ),
                    self._retrieval_eval_chunk(
                        chunk_id="w0-driver_cuda-install-a1b2c3d4",
                        product_area="driver_cuda",
                        title="CUDA 安装",
                        content="先安装 NVIDIA 驱动，再安装 CUDA Toolkit。",
                        question_patterns=["怎么安装 NVIDIA 驱动"],
                    ),
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "我远程连上 Windows 云服务器后没有声音",
                        "group": "windows_rdp_sound",
                        "product_area": "windows",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-windows-audio-a1b2c3d4"],
                    },
                    {
                        "question_id": "q2",
                        "question": "CUDA 装不上要先检查什么",
                        "group": "cuda_nvidia_driver",
                        "product_area": "driver_cuda",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-driver_cuda-install-a1b2c3d4"],
                    },
                ],
            )
            python_summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, retrieval_path)
            python_top3 = {
                row["question_id"]: [item["chunk_id"] for item in row["hit_items"]]
                for row in python_summary["trace_records"]
            }
            env = dict(os.environ)
            env.update(
                {
                    "RAG_RETRIEVER_PARITY_CHUNKS": str(chunks_path),
                    "RAG_RETRIEVER_PARITY_QUESTIONS": str(questions_path),
                    "RAG_RETRIEVER_PARITY_OUT": str(go_out_path),
                }
            )
            completed = subprocess.run(
                ["go", "test", "./internal/knowledge", "-run", "TestRetrieverParityFixture", "-count=1"],
                cwd=SCRIPTS_DIR.parent,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
            go_top3 = json.loads(go_out_path.read_text(encoding="utf-8"))

            self.assertEqual(go_top3, python_top3)

    def test_retriever_go_python_parity_fixture_with_cjk_extension(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            go_out_path = root / "go_top3.json"
            ext_a = chr(0x3402)
            ext_b = chr(0x2000B)
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-init_failure-rare-cjk-a1b2c3d4",
                        product_area="init_failure",
                        title=f"{ext_a}{ext_b} GPU 启动失败",
                        content=f"{ext_a}{ext_b} 卡型启动失败时，请检查镜像兼容性。",
                        question_patterns=[f"{ext_a}{ext_b} 卡启动失败怎么处理"],
                    ),
                    self._retrieval_eval_chunk(
                        chunk_id="w0-billing_rule-normal-a1b2c3d4",
                        product_area="billing_rule",
                        title="计费规则",
                        content="按量计费会根据资源使用情况扣费。",
                        question_patterns=["计费怎么算"],
                    ),
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": f"{ext_a}{ext_b} 这张卡启动失败了怎么办",
                        "group": "monitor_init_failure",
                        "product_area": "init_failure",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-init_failure-rare-cjk-a1b2c3d4"],
                    }
                ],
            )
            python_summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, retrieval_path)
            python_top3 = {
                row["question_id"]: [item["chunk_id"] for item in row["hit_items"]]
                for row in python_summary["trace_records"]
            }
            env = dict(os.environ)
            env.update(
                {
                    "RAG_RETRIEVER_PARITY_CHUNKS": str(chunks_path),
                    "RAG_RETRIEVER_PARITY_QUESTIONS": str(questions_path),
                    "RAG_RETRIEVER_PARITY_OUT": str(go_out_path),
                }
            )
            completed = subprocess.run(
                ["go", "test", "./internal/knowledge", "-run", "TestRetrieverParityFixture", "-count=1"],
                cwd=SCRIPTS_DIR.parent,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
            go_top3 = json.loads(go_out_path.read_text(encoding="utf-8"))

            self.assertEqual(go_top3, python_top3)

    def test_evaluate_retrieval_perfect_match(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            out_path = root / "retrieval_eval.json"
            chunk = self._retrieval_eval_chunk(
                chunk_id="w0-billing_rule-shutdown-a1b2c3d4",
                product_area="billing_rule",
                title="Shutdown billing",
                content="Shutdown billing follows the selected billing mode.",
                question_patterns=["Will shutdown still bill?"],
            )
            self._write_jsonl(chunks_path, [chunk])
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "Will shutdown still bill?",
                        "group": "billing_mode_shutdown",
                        "product_area": "billing_rule",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-billing_rule-shutdown-a1b2c3d4"],
                        "source_refs": ["billing-faq"],
                    }
                ],
            )

            summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, out_path)

            self.assertEqual(summary["questions_evaluated"], 1)
            self.assertEqual(summary["questions_excluded_non_answer_behavior"], 0)
            self.assertEqual(summary["top_3_hit_rate"], 1.0)
            self.assertEqual(summary["failed_questions"], [])

    def test_evaluate_retrieval_excludes_hard_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            out_path = root / "retrieval_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-billing_rule-shutdown-a1b2c3d4",
                        product_area="billing_rule",
                        title="Shutdown billing",
                        content="Shutdown billing follows the selected billing mode.",
                        question_patterns=["Will shutdown still bill?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "Can you check my real-time account balance?",
                        "group": "hard_block_account_finance",
                        "product_area": "billing_rule",
                        "expected_behavior": "hard_block",
                        "expected_chunk_ids": [],
                        "source_refs": [],
                    }
                ],
            )

            summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, out_path)

            self.assertEqual(summary["questions_evaluated"], 0)
            self.assertEqual(summary["questions_excluded_non_answer_behavior"], 1)
            self.assertIsNone(summary["top_3_hit_rate"])

    def test_evaluate_retrieval_emits_trace_v03_shape(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            out_path = root / "retrieval_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-init_failure-error-code-a1b2c3d4",
                        product_area="init_failure",
                        title="Error code table",
                        content="Init failure can be caused by unsupported image or insufficient resources.",
                        question_patterns=["instance init failure"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "instance init failure",
                        "group": "monitor_init_failure",
                        "product_area": "init_failure",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-init_failure-error-code-a1b2c3d4"],
                        "source_refs": ["gitlab-compshare-docs__compshareerrorcode"],
                    }
                ],
            )

            summary = evaluate_retrieval.evaluate_retrieval(chunks_path, questions_path, out_path)
            trace = summary["trace_records"][0]

            self.assertEqual(trace["query_raw"], "instance init failure")
            self.assertEqual(trace["query_normalized"], "instance init failure")
            self.assertEqual(trace["query_expansions"], [])
            self.assertEqual(trace["hits"], 1)
            self.assertEqual(trace["hit_items"][0]["chunk_id"], "w0-init_failure-error-code-a1b2c3d4")
            self.assertIsInstance(trace["hit_items"][0]["score"], float)
            self.assertTrue(trace["hit_items"][0]["kept"])

    def test_evaluate_answers_safety_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-login-ssh-a1b2c3d4",
                        product_area="login",
                        title="SSH login",
                        content="Use the console connection information to log in with SSH.",
                        question_patterns=["How do I log in with SSH?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I log in with SSH?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                        "source_refs": ["login-faq"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q1",
                                "hit_items": [{"chunk_id": "w0-login-ssh-a1b2c3d4", "score": 6.0, "kept": True}],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: "Use SSH from the console. [1]",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertEqual(summary["safety_failures"], 0)
            self.assertEqual(summary["internal_leakage"], 0)
            self.assertEqual(summary["answer_questions_evaluated"], 1)

    def test_evaluate_answers_counts_internal_leakage_from_chunks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            chunk = self._retrieval_eval_chunk(
                chunk_id="w0-login-spt-a1b2c3d4",
                product_area="login",
                title="SPT leak",
                content="Use SSH from the console.",
                question_patterns=["How do I log in?"],
            )
            chunk["source_refs"] = ["wxwork-spt-record-2026-05:case-1"]
            self._write_jsonl(chunks_path, [chunk])
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I log in with SSH?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-spt-a1b2c3d4"],
                        "source_refs": ["login-faq"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q1",
                                "hit_items": [{"chunk_id": "w0-login-spt-a1b2c3d4", "score": 6.0, "kept": True}],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: "Use SSH from the console. [1]",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertEqual(summary["safety_failures"], 0)
            self.assertEqual(summary["internal_leakage"], 1)

    def test_evaluate_answers_safety_fail(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-login-unsafe-a1b2c3d4",
                        product_area="login",
                        title="Unsafe login",
                        content="Use password: compshare123 to log in.",
                        question_patterns=["How do I log in?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I log in?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-unsafe-a1b2c3d4"],
                        "source_refs": ["login-faq"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q1",
                                "hit_items": [{"chunk_id": "w0-login-unsafe-a1b2c3d4", "score": 6.0, "kept": True}],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: "Use the password. [1]",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertGreater(summary["safety_failures"], 0)
            self.assertTrue(summary["failed_answers"])

    def test_evaluate_answers_judge_mock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-driver_cuda-install-a1b2c3d4",
                        product_area="driver_cuda",
                        title="CUDA install",
                        content="Install the NVIDIA driver before CUDA.",
                        question_patterns=["How do I install CUDA?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I install CUDA?",
                        "group": "cuda_nvidia_driver",
                        "product_area": "driver_cuda",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-driver_cuda-install-a1b2c3d4"],
                        "source_refs": ["cuda-runbook"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q1",
                                "hit_items": [{"chunk_id": "w0-driver_cuda-install-a1b2c3d4", "score": 6.0, "kept": True}],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: "Install the NVIDIA driver before CUDA. [1]",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertEqual(summary["grounded_rate"], 1.0)
            self.assertEqual(summary["cited_rate"], 1.0)
            self.assertEqual(summary["fabricated_rate"], 0.0)

    def test_evaluate_answers_retries_non_refusal_without_citation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-driver_cuda-install-a1b2c3d4",
                        product_area="driver_cuda",
                        title="CUDA install",
                        content="Install the NVIDIA driver before CUDA.",
                        question_patterns=["How do I install CUDA?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I install CUDA?",
                        "group": "cuda_nvidia_driver",
                        "product_area": "driver_cuda",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-driver_cuda-install-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q1",
                                "hit_items": [{"chunk_id": "w0-driver_cuda-install-a1b2c3d4", "score": 6.0, "kept": True}],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            calls: list[str] = []

            def answerer(question: str, chunks: list[dict]) -> str:
                calls.append(question)
                if len(calls) == 1:
                    return "Install the NVIDIA driver before CUDA."
                return "Install the NVIDIA driver before CUDA. [1]"

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=answerer,
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertEqual(len(calls), 2)
            self.assertIn("引用编号", calls[1])
            self.assertEqual(summary["cited_rate"], 1.0)
            self.assertEqual(summary["answer_questions_non_refused"], 1)
            self.assertTrue(summary["answers"][0]["citation_retry"])

    def test_evaluate_answers_excludes_refusal_from_citation_denominator(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-login-ssh-a1b2c3d4",
                        product_area="login",
                        title="SSH login",
                        content="Use SSH from the console.",
                        question_patterns=["How do I use SSH?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I resize a private disk?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {"question_id": "q1", "hit_items": [{"chunk_id": "w0-login-ssh-a1b2c3d4", "score": 6.0, "kept": True}]}
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: "知识库未覆盖这个问题。",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": False, "fabricated": False},
            )

            self.assertEqual(summary["answer_questions_refused"], 1)
            self.assertEqual(summary["answer_questions_non_refused"], 0)
            self.assertIsNone(summary["cited_rate"])
            self.assertEqual(summary["failed_answers"], [])

    def test_evaluate_answers_coerces_disclaimer_without_citation_to_refusal(self):
        """Step 6b runtime-parity update (2026-05-18) supersedes the RAG-13
        soft-disclaimer routing for the no-citation case.

        Original RAG-13 intent (2026-05-17): a disclaimer like
        '当前知识库只收录了以下信息：...' is a soft disclaimer, NOT a hard refusal,
        and is counted under `with_disclaimer`.

        Step 6b discovery: runtime engine.go:1074-1081 actually substitutes
        ragNoEvidenceReply when the citation retry produces neither a refusal
        template nor a [n] citation — including disclaimer-shaped text. The
        prior eval logic kept the disclaimer answer visible while runtime hid
        it, which made the eval STRICTER than production. Eval now mirrors
        runtime: disclaimer + retry-still-no-cite → coerced to pure refusal
        (memory feedback_eval_target_must_match_runtime_path).

        Disclaimers WITH citation are still counted under `with_disclaimer`
        (only the no-cite case is coerced).
        """
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-modelverse-package-a1b2c3d4",
                        product_area="modelverse",
                        title="Coding Plan quota",
                        content="Coding Plan has a fixed quota window.",
                        question_patterns=["Coding Plan quota"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How does Coding Plan quota work?",
                        "group": "modelverse_package_credit",
                        "product_area": "modelverse",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-modelverse-package-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {"question_id": "q1", "hit_items": [{"chunk_id": "w0-modelverse-package-a1b2c3d4", "score": 6.0, "kept": True}]}
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            calls: list[str] = []

            def answerer(question: str, chunks: list[dict]) -> str:
                calls.append(question)
                return "当前知识库只收录了以下信息：Coding Plan 有固定额度窗口。"

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=answerer,
                judge=lambda question, answer, chunks: {"grounded": True, "cited": False, "fabricated": False},
            )

            # Disclaimer + no [n] still triggers citation retry. Retry produces
            # the same disclaimer-no-cite content → coerced to ragNoEvidenceReply
            # per Step 6b runtime-parity update.
            self.assertEqual(len(calls), 2)
            ans0 = summary["answers"][0]
            self.assertTrue(ans0["pure_refusal"])
            self.assertEqual(ans0["answer"], evaluate_answers.RAG_NO_EVIDENCE_REPLY)
            self.assertTrue(ans0["retry_no_cite"])
            self.assertEqual(summary["answer_questions_pure_refused"], 1)
            self.assertEqual(summary["answer_questions_with_disclaimer"], 0)
            self.assertEqual(summary["answer_questions_non_refused"], 0)
            self.assertEqual(summary["retry_no_cite_count"], 1)
            self.assertEqual(summary["empty_answer_after_retry_count"], 0)
            # cited_rate denom = with_disclaimer + non_refused = 0; _rate(0,0) is None.
            self.assertIsNone(summary["cited_rate"])
            self.assertEqual(summary["pure_refusal_rate"], 1.0)
            self.assertEqual(summary["disclaimer_rate"], 0.0)
            self.assertEqual(summary["answer_questions_refused"], 1)
            # pure_refusal=True + judge grounded=True → no judge_flagged.
            self.assertEqual(summary.get("failed_answers"), [])

    # ------------------------------------------------------------------
    # RAG-13 metric split — 4 locked example cases (do not rely on length<100 alone)
    # ------------------------------------------------------------------

    def test_is_hard_refusal_matches_pure_refusal_template(self):
        """LOCKED CASE #1: pure refusal template.

        Short (<100 chars) + matches HARD_REFUSAL_RE phrase + no [n] citation.
        Three conditions must ALL hold; this case satisfies all three."""
        self.assertTrue(evaluate_answers._is_hard_refusal("知识库未覆盖这个问题。"))
        self.assertTrue(evaluate_answers._is_hard_refusal("当前知识库未覆盖该问题，我无法回答。"))
        self.assertTrue(evaluate_answers._is_hard_refusal("无法根据知识库回答这个问题。"))
        self.assertTrue(evaluate_answers._is_hard_refusal("没有找到可靠资料。"))

    def test_is_hard_refusal_excludes_short_answer_with_citation_and_disclaimer(self):
        """LOCKED CASE #2: short (~54 chars) substantive answer with [n] + disclaimer.

        Even though length<100, the presence of [n] proves the model returned a
        grounded answer — must NOT be classified as hard refusal. This is the
        user-caveat case ("错误码 226601") that pure-length rule would mis-classify."""
        answer = "错误码 226601 表示初始化失败 [1]。当前知识库只收录此一条。"
        self.assertLess(len(answer), 100)  # short enough to fool a length-only rule
        self.assertFalse(evaluate_answers._is_hard_refusal(answer))
        self.assertTrue(evaluate_answers._has_soft_disclaimer(answer))

    def test_is_hard_refusal_excludes_long_answer_with_disclaimer(self):
        """LOCKED CASE #3: long substantive answer + [n] + tail disclaimer.

        Length guard prevents matching even if the answer mentions a refusal phrase
        inside its body. Disclaimer counter should fire instead."""
        answer = (
            "根据资料，Ubuntu 系统下 SSH 端口为 22，社区镜像默认端口为 23。"
            "您可以在控制台查看实例详情中的端口信息。"
            "具体登录命令：ssh -p 22 ubuntu@<your-ip> [1][2]。"
            "若密码错误，请从控制台复制最新密码。"
            "当前知识库只收录了以上端口/账号信息，更多排错请联系工单。"
        )
        self.assertGreater(len(answer), 100)
        self.assertFalse(evaluate_answers._is_hard_refusal(answer))
        self.assertTrue(evaluate_answers._has_soft_disclaimer(answer))

    def test_is_hard_refusal_excludes_substantive_no_citation_no_refusal_phrase(self):
        """LOCKED CASE #4: long answer, no [n], no refusal/disclaimer phrase.

        Treated as "non_refused" — neither hard refusal nor disclaimer. The
        citation-retry loop will fire because [n] is absent, and (post-retry) the
        judge_flagged failure will record a `missing_citation` reason."""
        answer = (
            "您可以登录控制台查看实例状态。"
            "依次进入：1. 实例管理 2. 实例列表 3. 选择目标实例 4. 查看详情。"
            "如有疑问可联系客服。"
        )
        self.assertGreater(len(answer), 50)
        self.assertFalse(evaluate_answers._is_hard_refusal(answer))
        self.assertFalse(evaluate_answers._has_soft_disclaimer(answer))

    def test_has_soft_disclaimer_matches_boundary_phrases(self):
        """SOFT_DISCLAIMER_RE covers the four common boundary-disclosure variants."""
        self.assertTrue(evaluate_answers._has_soft_disclaimer("...答案 [1]。当前知识库只收录了以上信息。"))
        self.assertTrue(evaluate_answers._has_soft_disclaimer("...答案 [1]。知识库暂未收录其他细节。"))
        self.assertTrue(evaluate_answers._has_soft_disclaimer("当前知识库只覆盖这一项。"))
        self.assertTrue(evaluate_answers._has_soft_disclaimer("当前知识库未提供其他细节。"))
        # Non-disclaimer answer should not match.
        self.assertFalse(evaluate_answers._has_soft_disclaimer("...完整答案 [1]。"))
        # Pure hard refusal phrase alone is NOT a disclaimer.
        self.assertFalse(evaluate_answers._has_soft_disclaimer("知识库未覆盖这个问题。"))

    def test_answer_counts_separates_pure_refusal_disclaimer_non_refused(self):
        """_answer_counts must produce 3 distinct buckets, with cited only counted
        on the answered subset (disclaimer + non_refused), not on hard refusal."""
        items = [
            {"answer": "知识库未覆盖这个问题。", "judge": {"grounded": False}},
            {"answer": "Ubuntu 端口是 22 [1]。当前知识库只收录了以上信息。", "judge": {"grounded": True}},
            {"answer": "Ubuntu 端口是 22 [1]。完整答案。", "judge": {"grounded": True}},
        ]
        counts = evaluate_answers._answer_counts(items)
        self.assertEqual(counts["pure_refused"], 1)
        self.assertEqual(counts["with_disclaimer"], 1)
        self.assertEqual(counts["non_refused"], 1)
        # cited count covers disclaimer + non_refused — both have [1].
        self.assertEqual(counts["cited"], 2)
        # In-place mutation tags each item with pure_refusal + soft_disclaimer.
        self.assertTrue(items[0]["pure_refusal"])
        self.assertFalse(items[0]["soft_disclaimer"])
        self.assertFalse(items[1]["pure_refusal"])
        self.assertTrue(items[1]["soft_disclaimer"])
        self.assertFalse(items[2]["pure_refusal"])
        self.assertFalse(items[2]["soft_disclaimer"])

    def test_answer_summary_outputs_split_rates(self):
        """_answer_summary must emit new pure_refusal_rate + disclaimer_rate +
        explicit pure_refused / with_disclaimer / non_refused counts. cited_rate
        denominator now includes disclaimer answers (answered subset)."""
        summary = evaluate_answers._answer_summary(
            evaluated=3,
            grounded=2,
            cited=2,
            fabricated=0,
            safety_failures=0,
            internal_leakage=0,
            failed_answers=[],
            answer_model="x",
            judge_model="y",
            answer_results=[
                {"answer": "知识库未覆盖这个问题。", "judge": {"grounded": False}},
                {"answer": "答案 [1]。当前知识库只收录了以上信息。", "judge": {"grounded": True}},
                {"answer": "答案 [1]。", "judge": {"grounded": True}},
            ],
        )
        self.assertAlmostEqual(summary["pure_refusal_rate"], 1 / 3)
        self.assertAlmostEqual(summary["disclaimer_rate"], 1 / 3)
        self.assertEqual(summary["answer_questions_pure_refused"], 1)
        self.assertEqual(summary["answer_questions_with_disclaimer"], 1)
        self.assertEqual(summary["answer_questions_non_refused"], 1)
        # cited_rate denom = with_disclaimer + non_refused = 2; both cited → 1.0.
        self.assertEqual(summary["cited_rate"], 1.0)
        self.assertEqual(summary["citation_denominator"], 2)
        # Backward-compat alias: equals pure_refused (NOT pre-RAG-13 hard+soft).
        self.assertEqual(summary["answer_questions_refused"], 1)

    def test_evaluate_answers_marks_retry_still_missing_citation_false(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-billing_rule-4090-a1b2c3d4",
                        product_area="billing_rule",
                        title="4090 pricing",
                        content="4090 is billed hourly according to the selected billing mode.",
                        question_patterns=["4090 pricing"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How much is 4090 hourly?",
                        "group": "billing_mode_shutdown",
                        "product_area": "billing_rule",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-billing_rule-4090-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {"question_id": "q1", "hit_items": [{"chunk_id": "w0-billing_rule-4090-a1b2c3d4", "score": 6.0, "kept": True}]}
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            answers = [
                "4090 大概 8 元一小时,按量计费支持转包月。",
                "4090 价格每小时约 8 元,可以买包月。",
            ]

            def answerer(question: str, chunks: list[dict]) -> str:
                return answers.pop(0)

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=answerer,
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            # Step 6b runtime-parity update (2026-05-18): retry-still-no-cite now
            # mirrors engine.go:1081 — coerce to ragNoEvidenceReply and classify
            # as pure refusal. Previously the answer was kept and counted as
            # non_refused-but-uncited which made eval STRICTER than runtime
            # (memory feedback_eval_target_must_match_runtime_path).
            ans0 = summary["answers"][0]
            self.assertTrue(ans0["pure_refusal"])
            self.assertEqual(ans0["answer"], evaluate_answers.RAG_NO_EVIDENCE_REPLY)
            self.assertTrue(ans0["retry_no_cite"])
            self.assertFalse(ans0["empty_after_retry"])
            self.assertEqual(ans0["raw_retry_answer"], "4090 价格每小时约 8 元,可以买包月。")
            self.assertTrue(ans0["citation_retry"])
            self.assertEqual(summary["answer_questions_non_refused"], 0)
            self.assertEqual(summary["answer_questions_pure_refused"], 1)
            self.assertEqual(summary["retry_no_cite_count"], 1)
            self.assertEqual(summary["empty_answer_after_retry_count"], 0)
            # cited_rate denom (with_disclaimer + non_refused) is 0; _rate returns None.
            self.assertIsNone(summary["cited_rate"])
            # No judge-flagged entry: pure_refusal=True suppresses missing_citation flag
            # and the mock judge returns grounded=True with no fab.
            self.assertEqual(summary.get("failed_answers"), [])

    def test_evaluate_answers_coerces_empty_retry_to_refusal_and_counts_empty(self):
        """Step 6b runtime-parity (2026-05-18): when ds-v4-flash retry returns
        an empty string (observed at ~2% rate against ModelVerse on hybrid path),
        runtime engine.go:1081 still substitutes ragNoEvidenceReply. Eval must
        do the same AND surface the empty-return rate via
        empty_answer_after_retry_count so the API noise stays visible without
        polluting the cited_rate hard contract
        (memory feedback_eval_target_must_match_runtime_path)."""
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-billing_rule-4090-a1b2c3d4",
                        product_area="billing_rule",
                        title="4090 pricing",
                        content="4090 hourly billing per selected mode.",
                        question_patterns=["4090 pricing"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q-empty",
                        "question": "4090 多少钱",
                        "group": "billing_mode_shutdown",
                        "product_area": "billing_rule",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-billing_rule-4090-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q-empty",
                                "hit_items": [
                                    {"chunk_id": "w0-billing_rule-4090-a1b2c3d4", "score": 6.0, "kept": True}
                                ],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            # First answer: substantive but no [n]. Retry: empty (ds-v4-flash failure mode).
            answers = ["4090 是按小时计费的", ""]

            def answerer(question: str, chunks: list[dict]) -> str:
                return answers.pop(0)

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=answerer,
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            ans0 = summary["answers"][0]
            self.assertTrue(ans0["pure_refusal"])
            self.assertEqual(ans0["answer"], evaluate_answers.RAG_NO_EVIDENCE_REPLY)
            self.assertTrue(ans0["retry_no_cite"])
            self.assertTrue(ans0["empty_after_retry"])
            self.assertEqual(ans0["raw_retry_answer"], "")
            self.assertEqual(summary["answer_questions_pure_refused"], 1)
            self.assertEqual(summary["retry_no_cite_count"], 1)
            self.assertEqual(summary["empty_answer_after_retry_count"], 1)

    def test_evaluate_answers_retry_recovers_with_citation_no_coercion(self):
        """Step 6b runtime-parity (2026-05-18): when the citation retry yields
        a properly cited answer, the original answerer text is replaced with
        the retry result (existing behavior preserved) and NO ragNoEvidenceReply
        coercion happens. retry_no_cite stays False."""
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-billing_rule-4090-a1b2c3d4",
                        product_area="billing_rule",
                        title="4090 pricing",
                        content="4090 hourly billing per selected mode.",
                        question_patterns=["4090 pricing"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q-recover",
                        "question": "4090 多少钱",
                        "group": "billing_mode_shutdown",
                        "product_area": "billing_rule",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-billing_rule-4090-a1b2c3d4"],
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {
                                "question_id": "q-recover",
                                "hit_items": [
                                    {"chunk_id": "w0-billing_rule-4090-a1b2c3d4", "score": 6.0, "kept": True}
                                ],
                            }
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            # First answer: no [n]. Retry: cited.
            answers = ["4090 是按小时计费的", "4090 按小时计费 [1]。"]

            def answerer(question: str, chunks: list[dict]) -> str:
                return answers.pop(0)

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=answerer,
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            ans0 = summary["answers"][0]
            self.assertFalse(ans0["pure_refusal"])
            self.assertEqual(ans0["answer"], "4090 按小时计费 [1]。")
            self.assertTrue(ans0["citation_retry"])
            self.assertFalse(ans0["retry_no_cite"])
            self.assertFalse(ans0["empty_after_retry"])
            self.assertIsNone(ans0["raw_retry_answer"])
            self.assertEqual(summary["answer_questions_non_refused"], 1)
            self.assertEqual(summary["cited_rate"], 1.0)
            self.assertEqual(summary["retry_no_cite_count"], 0)
            self.assertEqual(summary["empty_answer_after_retry_count"], 0)

    def test_evaluate_answers_prompt_preserves_conflict_and_condition_rules(self):
        prompt = evaluate_answers._answer_prompt(
            "Coding Plan 的用量周期是怎么计算的",
            [
                {
                    "title": "额度重置机制：滚动 5 小时窗口",
                    "content": "Coding Plan 采用固定 5 小时窗口刷新额度。",
                }
            ],
        )

        self.assertIn("标题和正文存在冲突", prompt)
        self.assertIn("以正文中的明确陈述为准", prompt)
        self.assertIn("保留知识片段里的原始条件", prompt)
        self.assertIn("不要把示例改写成通用规则", prompt)

    def test_evaluate_answers_prompt_encodes_three_tier_disclaimer_strategy(self):
        """PR-RAG-Prompt-Disclaimer-Fix (2026-05-17): eval prompt must stay in
        sync with internal/prompt/rag.go and encode the same 3-tier rules.
        Pre-fix the eval prompt told the LLM to add 当前知识库只收录 whenever
        coverage was partial; the rule split here is what makes the new
        evaluate_answers metric (RAG-13) measurable against runtime behavior
        (memory feedback_eval_target_must_match_runtime_path)."""
        prompt = evaluate_answers._answer_prompt(
            "test q",
            [{"title": "t", "content": "c"}],
        )
        # Rule 1: complete hit -> strip disclaimer
        self.assertIn("完整回答", prompt)
        self.assertIn("直接给答案", prompt)
        self.assertIn("不要加", prompt)
        self.assertIn("当前知识库只收录", prompt)  # named in forbidden list
        self.assertIn("知识库暂未收录", prompt)  # both phrases forbidden
        # Rule 2: partial hit -> specific-gap natural wording
        self.assertIn("部分回答", prompt)
        self.assertIn("具体的限定词", prompt)
        self.assertIn("禁止", prompt)
        self.assertIn("无信息", prompt)
        # Rule 3: no hit -> pure refusal template
        self.assertIn("知识库未覆盖", prompt)

    def test_evaluate_answers_prompt_encodes_anti_fabrication_anchors(self):
        """Step 6b (2026-05-17): eval prompt must mirror internal/prompt/rag.go
        BuildRAGMessages and carry 6 anti-fabrication anchor bullets. Step 5
        controlled eval flagged 4 real fab cases under ds-v4-flash + BM25 and
        PR #94 hybrid eval flagged 2 more under ds-v4-pro + hybrid; the eval
        prompt must elicit the same anti-fab behavior as runtime or the
        post-fix fab metric will diverge from production
        (memory feedback_eval_target_must_match_runtime_path)."""
        prompt = evaluate_answers._answer_prompt(
            "test q",
            [{"title": "t", "content": "c"}],
        )
        anchors = {
            "code/import literal copy (0170 token corruption)": "字符级、按行原样复制知识片段",
            "enum/status-code literal copy (0267 token corruption)": "枚举值、常量名、错误码、HTTP 状态码必须按知识片段字面拷贝",
            "numeric literal copy (defensive against 0100)": "字面值复制(含小数点位数)",
            "no evidence-external suggestions (0259 extrapolate)": "故障排除建议、操作步骤、联系方式或下一步行动",
            "direction-word fidelity (0300 direction misread)": "方向性词汇时,必须按知识片段原始方向陈述",
            "field/list-title binding (0020 endpoint, 0028 deprecated list)": "字段或列表标题旁的具体值",
        }
        for purpose, phrase in anchors.items():
            self.assertIn(phrase, prompt, msg=f"anti-fab anchor for {purpose} missing")

    def test_evaluate_answers_resumes_existing_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            out_path = root / "answer_eval.json"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-login-ssh-a1b2c3d4",
                        product_area="login",
                        title="SSH login",
                        content="Use SSH from the console.",
                        question_patterns=["How do I use SSH?"],
                    ),
                    self._retrieval_eval_chunk(
                        chunk_id="w0-driver_cuda-install-a1b2c3d4",
                        product_area="driver_cuda",
                        title="CUDA install",
                        content="Install the NVIDIA driver before CUDA.",
                        question_patterns=["How do I install CUDA?"],
                    ),
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How do I use SSH?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                        "source_refs": ["login-faq"],
                    },
                    {
                        "question_id": "q2",
                        "question": "How do I install CUDA?",
                        "group": "cuda_nvidia_driver",
                        "product_area": "driver_cuda",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-driver_cuda-install-a1b2c3d4"],
                        "source_refs": ["cuda-runbook"],
                    },
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "trace_records": [
                            {"question_id": "q1", "hit_items": [{"chunk_id": "w0-login-ssh-a1b2c3d4", "score": 6.0, "kept": True}]},
                            {"question_id": "q2", "hit_items": [{"chunk_id": "w0-driver_cuda-install-a1b2c3d4", "score": 6.0, "kept": True}]},
                        ]
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            out_path.write_text(
                json.dumps(
                    {
                        "answer_questions_evaluated": 1,
                        "grounded_rate": 1.0,
                        "cited_rate": 1.0,
                        "fabricated_rate": 0.0,
                        "safety_failures": 0,
                        "internal_leakage": 0,
                        "failed_answers": [{"question_id": "q2", "reason": "model_call_error", "error": "transient"}],
                        "answer_model": "deepseek-v4-pro",
                        "judge_model": "claude-opus-4-7",
                        "answers": [
                            {
                                "question_id": "q1",
                                "answer": "Use SSH from the console. [1]",
                                "chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                                "judge": {"grounded": True, "cited": True, "fabricated": False},
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            calls: list[str] = []

            summary = evaluate_answers.evaluate_answers(
                chunks_path,
                questions_path,
                retrieval_path,
                out_path,
                answerer=lambda question, chunks: calls.append(question) or "Install CUDA. [1]",
                judge=lambda question, answer, chunks: {"grounded": True, "cited": True, "fabricated": False},
            )

            self.assertEqual(calls, ["How do I install CUDA?"])
            self.assertEqual(summary["answer_questions_evaluated"], 2)
            self.assertEqual(len(summary["answers"]), 2)
            self.assertEqual(summary["failed_answers"], [])

    def test_write_eval_report_promotes_only_when_gates_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            chunks_path = root / "chunks.jsonl"
            questions_path = root / "questions.jsonl"
            retrieval_path = root / "retrieval_eval.json"
            answer_path = root / "answer_eval.json"
            report_path = root / "eval_report.md"
            deploy_path = root / "deploy" / "kb" / "stage2b_w0.jsonl"
            self._write_jsonl(
                chunks_path,
                [
                    self._retrieval_eval_chunk(
                        chunk_id="w0-login-ssh-a1b2c3d4",
                        product_area="login",
                        title="SSH login",
                        content="Use SSH from the console.",
                        question_patterns=["How do I use SSH?"],
                    )
                ],
            )
            self._write_jsonl(
                questions_path,
                [
                    {
                        "question_id": "q1",
                        "question": "How can I connect with SSH from the console?",
                        "group": "remote_login_ssh_jupyter",
                        "product_area": "login",
                        "expected_behavior": "answer",
                        "expected_chunk_ids": ["w0-login-ssh-a1b2c3d4"],
                        "source_refs": ["login-faq"],
                        "is_anchor": True,
                    }
                ],
            )
            retrieval_path.write_text(
                json.dumps(
                    {
                        "questions_evaluated": 1,
                        "questions_excluded_non_answer_behavior": 0,
                        "top_3_hit_rate": 1.0,
                        "per_group_hit_rate": {"remote_login_ssh_jupyter": 1.0},
                        "failed_questions": [],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            answer_path.write_text(
                json.dumps(
                    {
                        "answer_questions_evaluated": 1,
                        "grounded_rate": 1.0,
                        "cited_rate": 1.0,
                        "fabricated_rate": 0.0,
                        "safety_failures": 0,
                        "internal_leakage": 0,
                        "failed_answers": [],
                        "answer_model": "deepseek-v4-pro",
                        "judge_model": "claude-opus-4-7",
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = write_eval_report.write_eval_report(
                chunks_path=chunks_path,
                questions_path=questions_path,
                retrieval_eval_path=retrieval_path,
                answer_eval_path=answer_path,
                report_path=report_path,
                deploy_path=deploy_path,
            )

            self.assertTrue(summary["passed"])
            self.assertTrue(deploy_path.exists())
            self.assertIn("Verdict: **PASS**", report_path.read_text(encoding="utf-8"))

            answer_path.write_text(
                json.dumps(
                    {
                        "answer_questions_evaluated": 1,
                        "grounded_rate": 1.0,
                        "cited_rate": 0.0,
                        "fabricated_rate": 0.0,
                        "safety_failures": 0,
                        "internal_leakage": 0,
                        "failed_answers": [{"question_id": "q1", "reason": "missing_citation"}],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = write_eval_report.write_eval_report(
                chunks_path=chunks_path,
                questions_path=questions_path,
                retrieval_eval_path=retrieval_path,
                answer_eval_path=answer_path,
                report_path=report_path,
                deploy_path=deploy_path,
            )

            self.assertFalse(summary["passed"])
            self.assertFalse(deploy_path.exists())
            self.assertFalse(summary["gates"]["cited"])

            answer_path.write_text(
                json.dumps(
                    {
                        "answer_questions_evaluated": 1,
                        "grounded_rate": 1.0,
                        "cited_rate": 1.0,
                        "fabricated_rate": 0.0,
                        "safety_failures": 1,
                        "internal_leakage": 0,
                        "failed_answers": [{"question_id": "q1", "reason": "safety_failure"}],
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )

            summary = write_eval_report.write_eval_report(
                chunks_path=chunks_path,
                questions_path=questions_path,
                retrieval_eval_path=retrieval_path,
                answer_eval_path=answer_path,
                report_path=report_path,
                deploy_path=deploy_path,
            )

            self.assertFalse(summary["passed"])
            self.assertFalse(deploy_path.exists())
            self.assertIn("Verdict: **FAIL**", report_path.read_text(encoding="utf-8"))

    def _retrieval_eval_chunk(
        self,
        *,
        chunk_id: str,
        product_area: str,
        title: str,
        content: str,
        question_patterns: list[str],
    ) -> dict:
        return {
            "chunk_id": chunk_id,
            "kb_version": "kb.test",
            "source_type": "faq",
            "source_origin": "official",
            "product_area": product_area,
            "acl": "customer_safe",
            "title": title,
            "question_patterns": question_patterns,
            "content": content,
            "source_refs": ["test-source"],
            "asset_refs": [],
            "confidence": "high",
            "valid_from": "2026-05-13",
            "evidence_kind": "knowledge",
            "surface_url": None,
            "retrieval_score_hint": None,
        }

    def _write_jsonl(self, path: Path, rows: list[dict]) -> None:
        path.write_text("".join(json.dumps(row, ensure_ascii=False) + "\n" for row in rows), encoding="utf-8")


if __name__ == "__main__":
    unittest.main()
