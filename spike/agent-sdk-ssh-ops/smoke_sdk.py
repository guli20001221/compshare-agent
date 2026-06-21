"""Minimal proof: Agent SDK harness -> claude CLI -> ccr gateway -> ds-v4-flash.
No SSH, no tools — just confirm the THIRD-PARTY model answers through the SDK harness."""
import os
import asyncio

os.environ.setdefault("ANTHROPIC_BASE_URL", "http://127.0.0.1:3456")
os.environ.setdefault("ANTHROPIC_API_KEY", "dummy-unused")
os.environ["NO_PROXY"] = "127.0.0.1,localhost"
os.environ["no_proxy"] = "127.0.0.1,localhost"

from claude_agent_sdk import query, ClaudeAgentOptions  # noqa: E402


async def main():
    opts = ClaudeAgentOptions(model="deepseek-v4-flash", max_turns=1)
    async for m in query(prompt="Reply with exactly one word: PONG", options=opts):
        cls = type(m).__name__
        if cls == "AssistantMessage":
            for b in getattr(m, "content", []) or []:
                if type(b).__name__ == "TextBlock":
                    print("ASSISTANT:", b.text)
        elif cls == "ResultMessage":
            print("RESULT usage:", getattr(m, "usage", None))
        else:
            print(cls)


if __name__ == "__main__":
    asyncio.run(main())
