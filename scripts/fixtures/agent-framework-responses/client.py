"""Exercise Orka with the real Agent Framework Responses client.

Install requirements.txt in a virtual environment. The Go integration test
starts Orka and a deterministic upstream; this client uses ordinary HTTP and
Agent Framework's own tool loop and session history.
"""

import argparse
import asyncio
import importlib.metadata
import os

from agent_framework import Agent
from agent_framework.openai import OpenAIChatClient


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--mode", choices=("coordinator", "client"), required=True)
    parser.add_argument("--stream", action="store_true")
    args = parser.parse_args()
    executions = 0

    def client_echo(text: str) -> str:
        """Echo the supplied text in the client process."""
        nonlocal executions
        executions += 1
        return "client result: " + text

    headers = {"X-Orka-Tools": "disabled"} if args.mode == "client" else {}
    client = OpenAIChatClient(
        model="fixture/test-model",
        # The local fixture does not authenticate. For a deployed Orka endpoint,
        # supply its bearer credential through ORKA_API_KEY.
        api_key=os.environ.get("ORKA_API_KEY", "local-fixture-only"),
        base_url=args.base_url,
        default_headers=headers,
    )
    agent = Agent(
        client=client,
        tools=[client_echo],
        default_options={
            "store": False,
            # This pinned client automatically includes encrypted reasoning.
            # Use the public SDK override to request only the supported output.
            "extra_body": {"include": []},
        },
    )
    session = agent.create_session()
    if args.stream:
        first = ""
        async for update in agent.run("Use a tool to obtain the fixture value.", session=session, stream=True):
            first += update.text
        followup = ""
        async for update in agent.run("Follow up using our earlier result.", session=session, stream=True):
            followup += update.text
    else:
        first = (await agent.run("Use a tool to obtain the fixture value.", session=session)).text
        followup = (await agent.run("Follow up using our earlier result.", session=session)).text
    assert "fixture final" in first, first
    assert "follow-up verified" in followup, followup
    assert executions == (1 if args.mode == "client" else 0), executions
    print(
        f"PASS mode={args.mode} stream={args.stream} client_tool_executions={executions} "
        f"agent-framework-core={importlib.metadata.version('agent-framework-core')} "
        f"agent-framework-openai={importlib.metadata.version('agent-framework-openai')} "
        f"openai={importlib.metadata.version('openai')}"
    )
    await client.client.close()


if __name__ == "__main__":
    asyncio.run(main())
