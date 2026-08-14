import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from scripts.rag_v2.gitlab_sync import (
    SOURCE_ID,
    SourceChange,
    build_incremental_candidate,
    changed_asset_issues,
    main,
    parse_name_status_z,
    read_jsonl,
    source_ref,
)
from scripts.rag_v2.pipeline import write_jsonl


def base_row(chunk_id: str, source: str, content: str) -> dict[str, object]:
    return {
        "chunk_id": chunk_id,
        "kb_version": "kb.platform.v2.old",
        "source_type": "faq",
        "source_origin": "official",
        "product_area": "resource_purchase",
        "acl": "customer_safe",
        "confidence": "high",
        "title": chunk_id,
        "content": content,
        "source_refs": [source],
    }


class GitLabSyncTests(unittest.TestCase):

    def _git(self, repo: Path, *args: str) -> str:
        result = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True, check=True)
        return result.stdout.strip()

    def test_parse_name_status_supports_add_modify_delete_and_rename(self):
        changes = parse_name_status_z(
            b"M\0pages/changed.md\0A\0pages/new.md\0D\0pages/gone.md\0R100\0pages/old.md\0pages/moved.md\0"
        )
        self.assertEqual(
            [
                SourceChange(action="M", old_path="pages/changed.md", new_path="pages/changed.md"),
                SourceChange(action="A", new_path="pages/new.md"),
                SourceChange(action="D", old_path="pages/gone.md"),
                SourceChange(action="R", old_path="pages/old.md", new_path="pages/moved.md"),
            ],
            changes,
        )

    def test_changed_non_decorative_image_requires_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            page = repo / "pages" / "workflow.md"
            page.parent.mkdir(parents=True)
            page.write_text("# 工作流\n\n![流程图](workflow.png)\n", encoding="utf-8")
            issues = changed_asset_issues(repo, [SourceChange(action="M", old_path="pages/workflow.md", new_path="pages/workflow.md")])
            self.assertEqual("non_decorative_image_requires_vl_release", issues[0]["reason"])

    def test_changed_decorative_image_does_not_block_text_delta(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            page = repo / "pages" / "workflow.md"
            page.parent.mkdir(parents=True)
            page.write_text("# 工作流\n\n![build](https://img.shields.io/badge/build-passing.svg)\n", encoding="utf-8")
            issues = changed_asset_issues(repo, [SourceChange(action="M", old_path="pages/workflow.md", new_path="pages/workflow.md")])
            self.assertEqual([], issues)

    def test_incremental_merge_preserves_frozen_faq_and_handles_amdr(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo = root / "docs"
            for name, body in {
                "changed.md": "# 修改后的文档\n\n新版本说明：qwen3-reranker-8b 可用于检索排序，配置项已经更新。",
                "new.md": "# 新文档\n\n这是一个新增的公开平台文档，说明 RTX 4090 可用规格与使用限制。",
                "moved.md": "# 改名后的文档\n\n这是一份已经移动路径的公开文档，内容足以形成新的 V2 chunk。",
                "untouched.md": "# 未修改文档\n\n这个文件用于验证运行时只替换发生变化的 GitLab source refs。",
            }.items():
                path = repo / "pages" / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(body, encoding="utf-8")

            base_corpus = root / "base.jsonl"
            write_jsonl(base_corpus, [
                base_row("frozen-faq", "faq-usage:优云智算使用问题FAQ.md", "冻结 FAQ 原始证据。"),
                base_row("changed-old", source_ref("pages/changed.md"), "待替换的旧文档证据。"),
                base_row("gone-old", source_ref("pages/gone.md"), "待删除的旧文档证据。"),
                base_row("rename-old", source_ref("pages/old.md"), "待改名的旧文档证据。"),
                base_row("untouched-old", source_ref("pages/untouched.md"), "未修改 GitLab 文档证据。"),
            ])
            result = build_incremental_candidate(
                docs_repo=repo,
                base_corpus=base_corpus,
                changes=[
                    SourceChange(action="M", old_path="pages/changed.md", new_path="pages/changed.md"),
                    SourceChange(action="A", new_path="pages/new.md"),
                    SourceChange(action="D", old_path="pages/gone.md"),
                    SourceChange(action="R", old_path="pages/old.md", new_path="pages/moved.md"),
                ],
                head_revision="abc123def456",
                kb_version="kb.platform.v2.test.abc123",
                valid_from="2026-08-02",
                out_dir=root / "candidate",
                semantic_client=None,
                semantic_model="unused",
            )

            self.assertEqual("candidate_ready", result.status)
            self.assertIsNotNone(result.candidate_path)
            rows = read_jsonl(result.candidate_path)
            ids = {str(row["chunk_id"]) for row in rows}
            self.assertIn("frozen-faq", ids)
            self.assertIn("untouched-old", ids)
            self.assertNotIn("changed-old", ids)
            self.assertNotIn("gone-old", ids)
            self.assertNotIn("rename-old", ids)
            self.assertTrue(any(source_ref("pages/changed.md") in row.get("source_refs", []) for row in rows))
            self.assertTrue(any(source_ref("pages/new.md") in row.get("source_refs", []) for row in rows))
            self.assertTrue(any(source_ref("pages/moved.md") in row.get("source_refs", []) for row in rows))
            self.assertTrue(all(row["kb_version"] == "kb.platform.v2.test.abc123" for row in rows))
            changed = next(row for row in rows if source_ref("pages/changed.md") in row.get("source_refs", []))
            self.assertEqual("abc123def456", changed["source_revision"])
            self.assertEqual(changed["document_id"], changed["parent_id"])
            self.assertGreater(changed["chunk_ordinal"], 0)
            manifest = json.loads((root / "candidate" / "gitlab_sync_manifest.json").read_text(encoding="utf-8"))
            self.assertEqual("candidate_ready", manifest["status"])
            self.assertEqual(5, manifest["merge"]["merged_rows"])

    def test_cli_reads_approved_state_but_only_writes_a_proposal(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo = root / "docs"
            repo.mkdir()
            self._git(repo, "init")
            self._git(repo, "config", "user.email", "rag-test@example.invalid")
            self._git(repo, "config", "user.name", "RAG Test")
            page = repo / "pages" / "changed.md"
            page.parent.mkdir()
            page.write_text("# 初始文档\n\n这是初始公开文档，内容长度满足 V2 收集条件。", encoding="utf-8")
            self._git(repo, "add", ".")
            self._git(repo, "commit", "-m", "base")
            base_revision = self._git(repo, "rev-parse", "HEAD")
            page.write_text("# 更新文档\n\n当前文档增加 qwen3-reranker-8b 配置说明，内容长度满足 V2 收集条件。", encoding="utf-8")
            self._git(repo, "add", ".")
            self._git(repo, "commit", "-m", "update")
            head_revision = self._git(repo, "rev-parse", "HEAD")

            base_corpus = root / "base.jsonl"
            write_jsonl(base_corpus, [base_row("old", source_ref("pages/changed.md"), "旧版本知识证据。")])
            state_path = root / "approved-state.json"
            approved_state = {
                "schema_version": "compshare.rag.gitlab-sync-state.v1",
                "head_revision": base_revision,
            }
            state_path.write_text(json.dumps(approved_state, ensure_ascii=False), encoding="utf-8")
            original_state = state_path.read_bytes()
            out_dir = root / "candidate"

            exit_code = main([
                "--docs-repo", str(repo),
                "--base-corpus", str(base_corpus),
                "--state-file", str(state_path),
                "--out-dir", str(out_dir),
                "--kb-version", "kb.platform.v2.test.state",
                "--valid-from", "2026-08-02",
                "--skip-semantic",
            ])

            self.assertEqual(0, exit_code)
            self.assertEqual(original_state, state_path.read_bytes(), "candidate generation must not advance approved state")
            proposal = json.loads((out_dir / "gitlab_sync_state_proposal.json").read_text(encoding="utf-8"))
            self.assertEqual(head_revision, proposal["head_revision"])
            self.assertTrue((out_dir / "stage2b_v2.jsonl").is_file())


if __name__ == "__main__":
    unittest.main()
