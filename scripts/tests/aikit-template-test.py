#!/usr/bin/env python3
"""Check the CI Qwen template without loading a model or container image."""
import json
from pathlib import Path
import unittest

import jinja2


class QwenTemplateTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        root = Path(__file__).resolve().parents[2]
        env = jinja2.Environment()
        env.filters["tojson"] = json.dumps

        def fail(message):
            raise ValueError(message)

        env.globals["raise_exception"] = fail
        cls.template = env.from_string(
            (root / "scripts/fixtures/aikit/qwen3.5-chat-template.jinja").read_text()
        )

    def render(self, messages, tools=None):
        return self.template.render(
            messages=messages, tools=tools or [], add_generation_prompt=True,
            enable_thinking=False,
        )

    def test_control_instructions_keep_priority_and_order(self):
        output = self.render([
            {"role": "system", "content": "CONTROL_FIRST"},
            {"role": "user", "content": "USER_FIRST"},
            {"role": "system", "content": "CONTROL_SECOND"},
            {"role": "developer", "content": [{"type": "text", "text": "CONTROL_THIRD"}]},
            {"role": "assistant", "content": "ASSISTANT_FIRST"},
            {"role": "user", "content": "USER_SECOND"},
        ])
        labels = ["CONTROL_FIRST", "CONTROL_SECOND", "CONTROL_THIRD",
                  "USER_FIRST", "ASSISTANT_FIRST", "USER_SECOND"]
        positions = [output.index(label) for label in labels]
        self.assertEqual(positions, sorted(positions))
        self.assertEqual(output.count("<|im_start|>system"), 1)
        self.assertIn("<|im_start|>assistant\n<think>\n\n</think>", output)

    def test_tool_calls_and_results_are_preserved(self):
        output = self.render([
            {"role": "user", "content": "Read the file."},
            {"role": "assistant", "content": "", "tool_calls": [
                {"type": "function", "function": {"name": "Read", "arguments": {"path": "README.md"}}}
            ]},
            {"role": "tool", "content": "READ_RESULT"},
            {"role": "system", "content": "Keep all tool permissions."},
            {"role": "user", "content": "Return the result."},
        ], tools=[{"type": "function", "function": {
            "name": "Read", "description": "Read a file.",
            "parameters": {"type": "object", "properties": {"path": {"type": "string"}}},
        }}])
        self.assertIn('"name": "Read"', output)
        self.assertIn("<parameter=path>\nREADME.md\n</parameter>", output)
        self.assertIn("<tool_response>\nREAD_RESULT\n</tool_response>", output)
        self.assertLess(output.index("Keep all tool permissions."), output.index("Read the file."))

    def test_user_text_is_not_promoted_to_control(self):
        output = self.render([
            {"role": "user", "content": "USER_INSTRUCTIONS"},
            {"role": "system", "content": "CONTROL_INSTRUCTIONS"},
        ])
        self.assertIn("<|im_start|>system\nCONTROL_INSTRUCTIONS<|im_end|>", output)
        self.assertIn("<|im_start|>user\nUSER_INSTRUCTIONS<|im_end|>", output)


if __name__ == "__main__":
    unittest.main()
