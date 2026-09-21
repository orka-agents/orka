"""Offline counterexamples for the demo's claims, using public API response shapes."""

import copy
import json
import os
import tempfile
import unittest
from pathlib import Path

import check


def history(task_name, with_remember=False):
    events = [{"type": "WorkerStarted"}]
    for iteration in range(1, 3 if with_remember else 2):
        count = int(with_remember and iteration == 1)
        events.extend([
            {"type": "ModelRequestStarted", "content": {"iteration": iteration, "messageCount": 1 if iteration == 1 else 3}},
            {"type": "ModelRequestCompleted", "content": {"iteration": iteration, "toolCalls": count}},
            {"type": "ModelMessage", "content": {"iteration": iteration, "toolCalls": count}},
        ])
        if count:
            events.extend({"type": event_type, "toolName": "remember", "toolCallID": "call-1"}
                          for event_type in ("ToolCallStarted", "ToolCallCompleted"))
    events.extend({"type": event_type} for event_type in ("ResultSubmitted", "WorkerCompleted", "TaskSucceeded"))
    for seq, event in enumerate(events, 1):
        event.update(seq=seq, taskName=task_name)
    return {"namespace": "demo", "streamID": task_name, "streamType": "task", "latestSeq": len(events), "events": events}


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.old_cwd = Path.cwd()
        os.chdir(self.directory.name)
        Path("question.txt").write_text("Where should returned replacement filters go, and which return label should we use?\n")
        self.run = {"namespace": "demo", "authorTask": "author", "beforeTask": "before", "afterTask": "after"}
        self.agent = {"spec": {"systemPrompt": {"inline": "Use only supplied reviewed memory. Do not call tools."}}}
        self.before = {"reviewedProcedureAvailable": False, "dock": None, "label": None,
                       "answer": "The reviewed return procedure is unavailable."}
        self.after = {"reviewedProcedureAvailable": True, "dock": "Dock 3", "label": "RET-4827",
                      "answer": "Send returned filters to Dock 3 with label RET-4827."}

    def tearDown(self):
        os.chdir(self.old_cwd)
        self.directory.cleanup()

    def task(self, stage):
        return {"metadata": {"name": stage, "namespace": "demo"}, "status": {"phase": "Succeeded"},
                "spec": {"type": "ai", "agentRef": {"name": "demo-memory-reader"},
                         "prompt": Path("question.txt").read_text().strip()}}

    def reader(self, stage="before", events=None, task=None, answer=None):
        return check.reader(task or self.task(stage), events or history(stage),
                            {"result": json.dumps(answer or getattr(self, stage))},
                            self.run, stage, self.agent)

    def test_valid_before_and_after(self):
        self.assertFalse(self.reader()["reviewedProcedureAvailable"])
        self.assertEqual(self.reader("after")["label"], "RET-4827")

    def test_late_tool_event_is_not_hidden_by_an_early_clean_page(self):
        events = history("before")
        events["events"].append({"seq": 8, "taskName": "before", "type": "ToolCallStarted", "toolName": "search_transcript"})
        events["latestSeq"] = 8
        with self.assertRaisesRegex(ValueError, "reader used a tool"):
            self.reader(events=events)

    def test_model_tool_request_rejects_missing_tool_event(self):
        events = history("before")
        events["events"][2]["content"]["toolCalls"] = 1
        with self.assertRaisesRegex(ValueError, "model requested a tool"):
            self.reader(events=events)

    def test_missing_page_rejected(self):
        events = history("before")
        del events["events"][3]
        with self.assertRaisesRegex(ValueError, "incomplete or out of order"):
            self.reader(events=events)

    def test_missing_model_response_rejected_even_with_contiguous_sequences(self):
        events = history("before")
        del events["events"][2]
        for seq, event in enumerate(events["events"], 1):
            event["seq"] = seq
        events["latestSeq"] -= 1
        with self.assertRaisesRegex(ValueError, "missing ModelRequestCompleted"):
            self.reader(events=events)

    def test_missing_worker_completion_rejected(self):
        events = history("before")
        events["events"][5]["type"] = "TaskPhaseChanged"
        with self.assertRaisesRegex(ValueError, "missing WorkerCompleted"):
            self.reader(events=events)

    def test_shared_conversation_rejected(self):
        task = self.task("before")
        task["spec"]["sessionRef"] = {"name": "original-conversation"}
        with self.assertRaisesRegex(ValueError, "unexpected task input: sessionRef"):
            self.reader(task=task)

    def test_hidden_initial_conversation_rejected(self):
        events = history("before")
        events["events"][1]["content"]["messageCount"] = 3
        with self.assertRaisesRegex(ValueError, "conversation history"):
            self.reader(events=events)

    def test_procedure_in_reader_configuration_rejected(self):
        self.agent["spec"]["systemPrompt"]["inline"] += " The label is RET-4827."
        with self.assertRaisesRegex(ValueError, "given the procedure directly"):
            self.reader()

    def test_existing_memory_rejected(self):
        with self.assertRaisesRegex(ValueError, "active shared memory already exists"):
            check.empty_memories({"items": [{"id": "earlier-memory"}]})

    def test_generic_or_wrong_recall_rejected(self):
        self.after["label"] = "RET-4828"
        with self.assertRaisesRegex(ValueError, "exact procedure"):
            self.reader("after")

    def test_plausible_invention_before_review_rejected(self):
        self.before["answer"] = "The reviewed procedure is unavailable, but try Dock 2."
        with self.assertRaisesRegex(ValueError, "unreviewed instructions"):
            self.reader()

    def test_summary_uses_linked_records_and_separate_review(self):
        Path("raw").mkdir()

        def save(name, value):
            Path(name).write_text(json.dumps(value))

        save("run.json", self.run)
        author = self.task("author")
        author["spec"]["agentRef"]["name"] = "demo-memory-author"
        author["spec"]["prompt"] = "Propose this procedure: Dock 3, RET-4827."
        save("author-task.json", author)
        save("raw/author-task.json", author)
        save("raw/author-events.json", history("author", with_remember=True))
        pending = {"id": "proposal-1", "namespace": "demo", "taskName": "author", "agentName": "demo-memory-author",
                   "type": "memory", "status": "pending", "content": "Returned filters go to Dock 3. Use RET-4827.",
                   "description": "Tags: warehouse-returns"}
        accepted = {**pending, "status": "accepted", "reviewer": "presenter", "reviewedAt": "2026-09-20T12:00:00Z"}
        applied = {**accepted, "status": "applied", "appliedMemoryId": "memory-1", "appliedBy": "presenter",
                   "appliedAt": "2026-09-20T12:01:00Z"}
        memory = {"id": "memory-1", "namespace": "demo", "taskName": "author", "agentName": "demo-memory-author",
                  "source": "memory_proposal", "sourceProposalId": "proposal-1", "content": pending["content"],
                  "tags": ["warehouse-returns"], "disabled": False, "deleted": False}
        save("raw/proposals.json", {"items": [pending]})
        for status, value in (("pending", pending), ("accepted", accepted), ("applied", applied)):
            save(f"raw/proposal-{status}.json", value)
        for stage in ("initial", "before", "accepted"):
            save(f"raw/memories-{stage}.json", {"items": None})
        for stage in ("applied", "after", "tagged"):
            save(f"raw/memories-{stage}.json", {"items": [memory]})
        save("raw/reader-agent.json", self.agent)
        save("raw/reader-agent-after.json", self.agent)
        for stage in ("before", "after"):
            save(f"raw/{stage}-task.json", self.task(stage))
            save(f"raw/{stage}-events.json", history(stage))
            save(f"raw/{stage}-result.json", {"result": json.dumps(getattr(self, stage))})
        check.check("summary")
        evidence = check.read("evidence.json")
        self.assertEqual(evidence["sourceProposalId"], evidence["proposal"])
        self.assertEqual(evidence["acceptedMemoryCount"], 0)
        wrong_memory = copy.deepcopy(memory)
        wrong_memory["sourceProposalId"] = "another-proposal"
        save("raw/memories-applied.json", {"items": [wrong_memory]})
        with self.assertRaisesRegex(ValueError, "not linked"):
            check.check("applied")
        save("raw/memories-accepted.json", {"items": [memory]})
        with self.assertRaisesRegex(ValueError, "active shared memory"):
            check.check("accepted")


if __name__ == "__main__":
    unittest.main()
