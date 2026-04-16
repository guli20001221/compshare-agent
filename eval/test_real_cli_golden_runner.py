import unittest


from eval.real_cli_golden_runner import (
    evaluate_case,
    normalize_to_steps,
    parse_session_output,
    validate_case_schema,
)


class ParseSessionOutputTests(unittest.TestCase):
    def test_parse_extracts_tools_display_and_reply(self):
        text = """
You>
  🔧 调用 RebootInstanceWorkflow ...
  ⚠️  即将执行变更操作: RebootInstanceWorkflow
  确认执行？(y/N)
  🔧 调用 RebootCompShareInstance ...
  ✅ RebootCompShareInstance 调用成功
  🔑 Jupyter Token: secret-token

Assistant> 实例 uhost-xxx 已成功重启。

You>
"""
        parsed = parse_session_output(text)
        self.assertEqual(parsed["tool_calls"], ["RebootInstanceWorkflow", "RebootCompShareInstance"])
        self.assertTrue(parsed["has_confirm"])
        self.assertEqual(parsed["display_lines"], ["Jupyter Token: secret-token"])
        self.assertEqual(parsed["assistant_reply"], "实例 uhost-xxx 已成功重启。")


class EvaluateCaseTests(unittest.TestCase):
    def test_explicit_id_first_tool_must_match(self):
        case = {
            "id": "explicit-id",
            "expect_tool_calls": ["RebootInstanceWorkflow"],
            "expect_first_tool": "RebootInstanceWorkflow",
        }
        parsed = {
            "tool_calls": ["DescribeCompShareInstance", "RebootCompShareInstance"],
            "display_lines": [],
            "assistant_reply": "这里是实例详情。",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(case, parsed)
        self.assertFalse(result["passed"])
        self.assertTrue(any("first tool" in failure for failure in result["failures"]))

    def test_knowledge_case_passes_without_tool_calls(self):
        case = {
            "id": "knowledge",
            "expect_no_tool_call": True,
            "reply_contains": ["无卡", "控制台"],
        }
        parsed = {
            "tool_calls": [],
            "display_lines": [],
            "assistant_reply": "无卡模式是关机后以无卡模式启动实例，费用远低于正常开机，具体以控制台为准。",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(case, parsed)
        self.assertTrue(result["passed"])
        self.assertEqual(result["failures"], [])

    def test_any_of_reply_keywords_accepts_equivalent_instance_reference(self):
        case = {
            "id": "port-diagnose",
            "expect_no_tool_call": True,
            "reply_contains_any": ["ID", "uhost-"],
        }
        parsed = {
            "tool_calls": [],
            "display_lines": [],
            "assistant_reply": "请告诉我哪个实例的JupyterLab无法打开：1. uhost-1p1r57tl3cmw (host)",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(case, parsed)
        self.assertTrue(result["passed"])

    def test_any_of_reply_keywords_fails_when_none_match(self):
        case = {
            "id": "port-diagnose",
            "expect_no_tool_call": True,
            "reply_contains_any": ["ID", "uhost-"],
        }
        parsed = {
            "tool_calls": [],
            "display_lines": [],
            "assistant_reply": "请告诉我哪台实例的 JupyterLab 无法打开。",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(case, parsed)
        self.assertFalse(result["passed"])


class SchemaValidationTests(unittest.TestCase):
    def test_valid_single_turn(self):
        validate_case_schema({"id": "test", "input": "hello"})

    def test_valid_multi_turn(self):
        validate_case_schema({"id": "test", "steps": [{"input": "hi"}]})

    def test_both_input_and_steps_raises(self):
        with self.assertRaises(ValueError):
            validate_case_schema({"id": "test", "input": "hi", "steps": []})

    def test_neither_input_nor_steps_raises(self):
        with self.assertRaises(ValueError):
            validate_case_schema({"id": "test"})


class NormalizeToStepsTests(unittest.TestCase):
    def test_single_turn_wraps_assertions(self):
        case = {
            "id": "t1", "input": "hello", "confirm": "y",
            "expect_tool_calls": ["FooTool"],
            "reply_contains": ["bar"],
        }
        steps = normalize_to_steps(case)
        assert len(steps) == 1
        assert steps[0]["input"] == "hello"
        assert steps[0]["confirm"] == "y"
        assert steps[0]["expect_tool_calls"] == ["FooTool"]
        assert steps[0]["reply_contains"] == ["bar"]

    def test_multi_turn_returns_steps_directly(self):
        case = {
            "id": "t2",
            "steps": [
                {"input": "a", "reply_contains": ["x"]},
                {"input": "b", "expect_no_tool_call": True},
            ],
        }
        steps = normalize_to_steps(case)
        assert len(steps) == 2
        assert steps[0]["input"] == "a"
        assert steps[1]["expect_no_tool_call"] is True


class EvaluateStepTests(unittest.TestCase):
    def test_evaluate_works_on_step_dict(self):
        step = {
            "input": "关机吧",
            "expect_no_tool_call": True,
            "reply_contains": ["哪"],
        }
        parsed = {
            "tool_calls": [],
            "display_lines": [],
            "assistant_reply": "请问您要关哪台实例？",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(step, parsed)
        self.assertTrue(result["passed"])
        self.assertEqual(result["failures"], [])

    def test_evaluate_step_fails_on_unexpected_tool(self):
        step = {
            "input": "关机吧",
            "expect_no_tool_call": True,
            "reject_tool_calls": ["StopInstanceWorkflow"],
        }
        parsed = {
            "tool_calls": ["StopInstanceWorkflow"],
            "display_lines": [],
            "assistant_reply": "已关机",
            "errors": [],
            "has_confirm": False,
        }
        result = evaluate_case(step, parsed)
        self.assertFalse(result["passed"])
        self.assertTrue(any("no tool call" in f for f in result["failures"]))


if __name__ == "__main__":
    unittest.main()
