#!/usr/bin/env python3
"""Write the two Tools, Agent, and requests for one demo run as Kubernetes JSON."""

import argparse
import json
from pathlib import Path
from urllib.parse import urlencode


def prepare(directory, namespace, run, provider, model):
    directory = Path(directory)
    labels = {"demo.orka.ai/name": "09-governed-tools", "demo.orka.ai/run": run}

    def resource(kind, name, spec):
        return {
            "apiVersion": "core.orka.ai/v1alpha1",
            "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": labels.copy()},
            "spec": spec,
        }

    stock, order, agent = f"supplier-stock-{run}", f"supplier-order-{run}", f"supplier-assistant-{run}"
    item = {"type": "string", "enum": ["replacement-filter"], "description": "The item to check or order."}
    stock_tool = resource("Tool", stock, {
        "description": "Check how many replacement filters the supplier has available. Does not place an order.",
        "parameters": {"type": "object", "properties": {"item": item}, "required": ["item"], "additionalProperties": False},
        "http": {"method": "GET", "url": "https://example.com/v1/stock?" + urlencode({"run": run}) + "&item={{item}}",
                 "outboundAccessPolicyRef": {"name": "demo-supplier-gateway"}},
    })
    order_tool = resource("Tool", order, {
        "description": "Request a supplier order. A rejected request is not a purchase; report the service's refusal.",
        "parameters": {"type": "object", "properties": {"item": item,
                        "quantity": {"type": "integer", "minimum": 1, "maximum": 100}},
                       "required": ["item", "quantity"], "additionalProperties": False},
        "http": {"method": "POST", "url": "https://example.com/v1/orders?" + urlencode({"run": run}),
                 "outboundAccessPolicyRef": {"name": "demo-supplier-gateway"}},
    })
    assistant = resource("Agent", agent, {
        "providerRef": {"name": provider}, "model": {"name": model, "temperature": 0},
        "tools": [{"name": stock}, {"name": order}],
        "systemPrompt": {"inline": (
            "You help an inventory team check its supplier. Make exactly one call to the supplier tool requested "
            "in the task. Use only that result and the task's facts. Do not use memory, transcript, or other tools. "
            "Do not retry a refused operation or substitute another tool. Report the actual response in at most "
            "two sentences, using digits for quantities. Never claim an order was placed if the tool failed."
        )},
    })
    shortage = "Check whether our supplier has 20 replacement filters available."
    requests = {
        "lookup": shortage + f" Use {stock} for item replacement-filter. State the quantity returned and whether it covers 20 filters.",
        "order": f"Use {order} to request 20 replacement filters, item replacement-filter. Report the supplier response. Stop after one attempt, including a refusal.",
    }
    for name, obj in (("tools-and-agent.json", {"apiVersion": "v1", "kind": "List", "items": [stock_tool, order_tool, assistant]}),
                      ("stock-tool.json", stock_tool), ("order-tool.json", order_tool)):
        (directory / name).write_text(json.dumps(obj, indent=2) + "\n")
    for action, prompt in requests.items():
        task = resource("Task", f"supplier-{action}-{run}", {
            "type": "ai", "agentRef": {"name": agent}, "prompt": prompt, "timeout": "5m",
        })
        (directory / f"{action}.json").write_text(json.dumps(task, indent=2) + "\n")
    (directory / "request.txt").write_text(shortage + "\n")
    (directory / "run.json").write_text(json.dumps({
        "runId": run, "namespace": namespace, "stockTool": stock, "orderTool": order,
        "agent": agent, "lookupTask": f"supplier-lookup-{run}", "orderTask": f"supplier-order-{run}",
        "item": "replacement-filter", "requested": 20,
    }, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("directory", "namespace", "run", "provider", "model"):
        parser.add_argument(name)
    args = parser.parse_args()
    prepare(args.directory, args.namespace, args.run, args.provider, args.model)


if __name__ == "__main__":
    main()
