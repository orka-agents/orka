#!/usr/bin/env python3
"""Restore recurrent-prefix checkpoints in the pinned LocalAI backend."""
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
grpc = root / "backend/cpp/llama-cpp/grpc-server.cpp"
text = grpc.read_text()
anchor = """#endif
                task.id_slot = json_value(data, "id_slot", -1);"""
replacement = """#endif
                // Match llama.cpp's HTTP path: recurrent prefix checkpoints need
                // the message boundaries returned by the chat template.
                auto delimiters = common_chat_msg_delimiters_parse(
                    json_value(data, "message_delimiters", json::array()));
                delimiters.tokenize(ctx_server.impl->vocab);
                task.params.message_spans = task.tokens.find_message_spans(delimiters);
                task.id_slot = json_value(data, "id_slot", -1);"""
if text.count(anchor) != 2:
    raise SystemExit("pinned LocalAI completion anchors do not match")
patched_grpc = text.replace(anchor, replacement)

context = root / "backend/cpp/llama-cpp/llama.cpp/tools/server/server-context.cpp"
text = context.read_text()
anchor = """                        if (!is_user_start && !is_score_boundary && !near_prompt_end) {
                            do_checkpoint = false;
                        }"""
replacement = """                        // Preserve score behavior; completion system prompts may contain
                        // changing context before the first user message.
                        if (slot.task->type != SERVER_TASK_TYPE_COMPLETION &&
                            !is_user_start && !is_score_boundary && !near_prompt_end) {
                            do_checkpoint = false;
                        }"""
if text.count(anchor) != 1:
    raise SystemExit("prepared llama.cpp checkpoint anchor does not match")
patched_context = text.replace(anchor, replacement)
# prepare.sh stages these sources before this correction is applied.
staged = context.parent.parent / "grpc-server"
if not (staged / "grpc-server.cpp").is_file() or not (staged / "server-context.cpp").is_file():
    raise SystemExit("LocalAI prepare.sh must run before checkpoint correction")
grpc.write_text(patched_grpc)
context.write_text(patched_context)
(staged / "grpc-server.cpp").write_text(patched_grpc)
(staged / "server-context.cpp").write_text(patched_context)
