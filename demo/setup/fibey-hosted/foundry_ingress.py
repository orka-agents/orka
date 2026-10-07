"""Run AgentKit's brokered Responses server behind Foundry's hosted ingress.

agentkit-foundry-brokered refuses a public bind without its own bearer token,
which Foundry's Entra-authenticated ingress does not send. Keep AgentKit on
loopback, as it intends, and forward the hosted port to it byte for byte.
Foundry's ingress remains the only network path into the container; brokered
continuations still require AgentKit's continuation proof.
"""

import asyncio
import os
import signal
import sys

PUBLIC_PORT = int(os.environ.get("PORT", "8088"))
AGENTKIT_PORT = 8089


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        writer.close()


async def forward(client_reader, client_writer):
    try:
        upstream_reader, upstream_writer = await asyncio.open_connection("127.0.0.1", AGENTKIT_PORT)
    except OSError:
        client_writer.close()
        return
    await asyncio.gather(pipe(client_reader, upstream_writer), pipe(upstream_reader, client_writer))


async def main():
    agentkit = await asyncio.create_subprocess_exec(
        "/opt/agentkit/bin/agentkit-foundry-brokered", "--config", "/agent/agent.yaml",
        "--host", "127.0.0.1", "--port", str(AGENTKIT_PORT))
    server = await asyncio.start_server(forward, "0.0.0.0", PUBLIC_PORT)
    loop = asyncio.get_running_loop()
    stop = asyncio.Event()
    for signum in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(signum, stop.set)
    exited = asyncio.ensure_future(agentkit.wait())
    stopped = asyncio.ensure_future(stop.wait())
    await asyncio.wait({exited, stopped}, return_when=asyncio.FIRST_COMPLETED)
    server.close()
    if agentkit.returncode is None:
        agentkit.terminate()
        await agentkit.wait()
    sys.exit(agentkit.returncode or 0)


if __name__ == "__main__":
    asyncio.run(main())
