import json
from pathlib import Path
import re
import sys
import tempfile
import unittest

SCRIPTS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS_DIR))

from rag_w0 import build_source_manifest
from rag_w0 import clean_docs
from rag_w0 import chunk_docs
from rag_w0 import classify_links
from rag_w0 import common
from rag_w0 import describe_images
from rag_w0 import extract_assets
from rag_w0 import generate_eval_questions
from rag_w0 import mine_internal_cases
from rag_w0 import model_smoke
from rag_w0 import normalize_docs
from rag_w0 import select_w0_sources
from rag_w0 import snapshot_assets
from rag_w0 import snapshot_links
from rag_w0 import validate_case_approvals
from rag_w0 import validate_chunks
from rag_w0 import validate_cleaned_docs
from rag_w0 import validate_source


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
                        "redacted_text": "资源id：[RESOURCE_ID_REDACTED] 客户问题：[图片]初始化失败",
                    },
                    ensure_ascii=False,
                )
                + "\n",
                encoding="utf-8",
            )
            out = root / "golden_questions.jsonl"

            summary = generate_eval_questions.generate_eval_questions(chunks_path, out, min_questions=50, cases_path=cases_path)

            questions = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
            behaviors = {item["expected_behavior"] for item in questions}
            groups = {item["group"] for item in questions}
            self.assertGreaterEqual(summary["question_count"], 50)
            self.assertTrue({"answer", "refuse", "hard_block", "escalate"}.issubset(behaviors))
            self.assertTrue(all(item["expected_behavior"] in {"answer", "refuse", "hard_block", "escalate"} for item in questions))
            self.assertTrue(all(item["group"] in generate_eval_questions.EXPECTED_GROUPS for item in questions))
            self.assertTrue(generate_eval_questions.EXPECTED_GROUPS.issubset(groups))
            self.assertTrue(any(item["expected_chunk_ids"] == ["w0-login-001"] for item in questions))
            mined = [item for item in questions if item["source_refs"] == ["wxwork-spt-record-2026-05:case-0001"]]
            self.assertEqual(len(mined), 1)
            self.assertEqual(mined[0]["question"], "实例初始化失败时应该怎么处理？")
            self.assertNotIn("PERSON_REDACTED", mined[0]["question"])


if __name__ == "__main__":
    unittest.main()
