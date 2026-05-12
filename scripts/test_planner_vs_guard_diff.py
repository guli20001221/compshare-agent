import importlib.util
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("planner_vs_guard_diff.py")
TESTDATA = Path(__file__).with_name("testdata") / "planner_vs_guard_trace.jsonl"


def load_module():
    spec = importlib.util.spec_from_file_location("planner_vs_guard_diff", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PlannerVsGuardDiffTests(unittest.TestCase):
    def test_summary_counts_planner_guard_and_monitor_freshness(self):
        mod = load_module()

        records = mod.load_records(TESTDATA)
        summary = mod.summarize(records)

        self.assertEqual(summary["total_turns"], 6)
        self.assertEqual(summary["planner_enabled_turns"], 5)
        self.assertEqual(summary["schema_valid_turns"], 5)
        self.assertEqual(summary["invalid_or_fallback_turns"], 0)
        self.assertEqual(summary["engine_hard_block_count"], 1)
        self.assertEqual(summary["account_hard_block"]["matched"], 1)
        self.assertEqual(summary["account_hard_block"]["mismatched"], 1)
        self.assertEqual(summary["monitor_freshness_miss_count"], 1)
        self.assertEqual(summary["intent_counts"]["monitor_query"], 2)
        self.assertEqual(summary["intent_counts"]["billing_account_unsupported"], 2)
        self.assertEqual(summary["boundary_counts"]["diagnosis"], 1)

    def test_markdown_report_contains_required_sections(self):
        mod = load_module()
        summary = mod.summarize(mod.load_records(TESTDATA))

        report = mod.render_markdown(summary, source=TESTDATA)

        self.assertIn("# Planner vs Runtime Report", report)
        self.assertIn("Total turns | 6", report)
        self.assertIn("Planner-enabled turns | 5", report)
        self.assertIn("Schema-valid rate | 100.00%", report)
        self.assertIn("Monitor freshness misses | 1", report)
        self.assertIn("| billing_account_unsupported | 2 |", report)
        self.assertIn("Account Hard-Block Agreement", report)
        self.assertIn("Boundary Outcomes", report)
        self.assertIn("| diagnosis | 1 |", report)

    def test_summary_keeps_legacy_mixed_boundary_compatibility(self):
        mod = load_module()

        summary = mod.summarize([
            {
                "trace_id": "legacy",
                "turn_index": 1,
                "planner": {
                    "enabled": True,
                    "schema_valid": True,
                    "intent": "mixed_diagnosis_kb",
                },
            },
            {
                "trace_id": "legacy-billing",
                "turn_index": 2,
                "planner": {
                    "enabled": True,
                    "schema_valid": True,
                    "intent": "billing_instance",
                },
            },
            {
                "trace_id": "new-diagnosis",
                "turn_index": 3,
                "planner": {
                    "enabled": True,
                    "schema_valid": True,
                    "intent": "diagnosis",
                },
            },
        ])

        self.assertEqual(summary["boundary_counts"]["mixed_diagnosis_kb"], 1)
        self.assertEqual(summary["mixed_boundary_counts"]["mixed_diagnosis_kb"], 1)
        self.assertEqual(summary["mixed_boundary_counts"]["billing_instance"], 1)
        self.assertNotIn("diagnosis", summary["mixed_boundary_counts"])

    def test_cli_writes_markdown_report(self):
        mod = load_module()
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "report.md"

            rc = mod.main([str(TESTDATA), "--output", str(out)])

            self.assertEqual(rc, 0)
            text = out.read_text(encoding="utf-8")
            self.assertIn("Monitor freshness misses | 1", text)


if __name__ == "__main__":
    unittest.main()
