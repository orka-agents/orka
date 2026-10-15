"""Validate actual HTTP/SSE payloads with the pinned OpenAI SDK schemas."""

import json
import sys

from openai.types.responses import Response
from openai.types.responses.response_stream_event import ResponseStreamEvent
from pydantic import TypeAdapter

stream_event = TypeAdapter(ResponseStreamEvent)
for payload in json.load(sys.stdin):
    if payload.get("object") == "response":
        Response.model_validate(payload)
    else:
        stream_event.validate_python(payload)
print("PASS pinned OpenAI response and SSE schemas")
