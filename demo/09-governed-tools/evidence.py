#!/usr/bin/env python3
"""Check saved run records before displaying a supplier-demo claim."""

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.parse import parse_qs, urlsplit


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(name):
    return json.loads(Path(name).read_text())


def gateway_requests(path, run):
    requests = []
    for line in Path(path).read_text().splitlines():
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(record, dict):
            continue
        fields = record.get("fields", record)
        request = fields.get("demo_request")
        if not isinstance(request, str):
            continue
        parsed = urlsplit(request)
        if parse_qs(parsed.query).get("run") != [run]:
            continue
        requests.append({"method": fields.get("demo_method"), "path": parsed.path,
                         "status": fields.get("demo_status"), "request": request})
    return requests


def supplier_record(name, config):
    record = read("raw/" + name + ".json")
    require(record["runId"] == config["runId"], "supplier receipt belongs to another run")
    require(bool(record.get("instanceId")), "supplier instance identity missing")
    require(record["totalOrderRequests"] == 0 and record["totalOrdersCreated"] == 0,
            "an order request reached the supplier; this run cannot demonstrate a blocked purchase")
    require(record["orderRequests"] == 0 and record["ordersCreated"] == 0, "order counts are not zero")
    return record


def check_initial(config):
    before = supplier_record("supplier-before", config)
    require(before["receipts"] == [], "this run ID already has supplier receipts")
    return before


def tool_events(action, config, outcome):
    task = read(f"raw/{action}-task.json")
    expected_task = config[action + "Task"]
    require(task["metadata"]["name"] == expected_task and bool(task["metadata"].get("uid")), "wrong Task identity")
    require(task["metadata"]["namespace"] == config["namespace"], "wrong Task namespace")
    require(task["spec"]["type"] == "ai" and task["spec"]["agentRef"]["name"] == config["agent"], "wrong Agent or Task type")
    require(task["status"]["phase"] == "Succeeded", "the agent Task did not succeed")
    stream = read(f"raw/{action}-events.json")
    require(stream["streamID"] == expected_task and stream["namespace"] == config["namespace"], "wrong event stream")
    events = stream["events"]
    require([e["seq"] for e in events] == list(range(1, stream["latestSeq"] + 1)), "event history is incomplete")
    require(any(e["type"] == "TaskSucceeded" for e in events), "terminal event missing")
    starts = [e for e in events if e["type"] == "ToolCallStarted"]
    finishes = [e for e in events if e["type"] in ("ToolCallCompleted", "ToolCallFailed")]
    require(len(starts) == len(finishes) == 1, "expected exactly one supplier call and its result")
    tool = config["stockTool" if action == "lookup" else "orderTool"]
    require(starts[0]["toolName"] == finishes[0]["toolName"] == tool, "unexpected tool used")
    require(bool(starts[0].get("toolCallID")) and starts[0]["toolCallID"] == finishes[0]["toolCallID"], "tool result is not correlated")
    require(finishes[0]["type"] == outcome, "unexpected tool outcome")
    return finishes[0]


def check_lookup(config):
    before = check_initial(config)
    receipt = supplier_record("supplier-lookup", config)
    require(receipt["instanceId"] == before["instanceId"], "supplier restarted and lost its receipts")
    require(len(receipt["receipts"]) == 1, "expected exactly one supplier request")
    stock = receipt["receipts"][0]
    require(stock["method"] == "GET" and stock["path"] == "/v1/stock" and stock["status"] == 200,
            "supplier did not receive the stock request")
    require(stock["credentialAccepted"] is True and stock["orderCreated"] is False, "supplier did not accept gateway credential")
    require(stock["item"] == config["item"] and type(stock.get("available")) is int and stock["available"] == 32, "unexpected supplier stock")
    tool_events("lookup", config, "ToolCallCompleted")
    gateway = gateway_requests("raw/gateway-lookup.jsonl", config["runId"])
    require(len(gateway) == 1 and gateway[0]["method"] == "GET" and gateway[0]["path"] == "/v1/stock"
            and gateway[0]["status"] == 200, "gateway's successful stock request is missing")
    answer = read("raw/lookup-result.json")["result"]
    require(all(re.search(r"\b" + str(n) + r"\b", answer) for n in (stock["available"], config["requested"])),
            "agent answer did not state the returned stock and requested quantity")
    return {"operation": "Stock lookup", "gatewayStatus": gateway[0]["status"],
            "authenticated": stock["credentialAccepted"], "available": stock["available"]}


def check_order(config):
    lookup = check_lookup(config)
    receipt = supplier_record("supplier-after", config)
    stock_receipt = read("raw/supplier-lookup.json")
    require(receipt["instanceId"] == stock_receipt["instanceId"], "supplier restarted during the order attempt")
    require(receipt["receipts"] == stock_receipt["receipts"], "another request reached the supplier after the lookup")
    event = tool_events("order", config, "ToolCallFailed")
    require(event["summary"] == "gateway returned HTTP 404", "tool failure is not the gateway's no-route refusal")
    gateway = gateway_requests("raw/gateway-order.jsonl", config["runId"])
    orders = [r for r in gateway if r["method"] == "POST" and r["path"] == "/v1/orders"]
    require(len(gateway) == 2 and len(orders) == 1 and orders[0]["status"] == 404,
            "the order request did not reach the gateway and receive HTTP 404")
    require(bool(read("raw/order-result.json")["result"].strip()), "agent's explanation is missing")
    return {"lookup": lookup, "order": {"operation": "Place order", "gatewayStatus": orders[0]["status"],
            "refusal": event["summary"], "supplierRequests": receipt["orderRequests"], "ordersCreated": receipt["ordersCreated"]}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("initial", "lookup", "order", "order-result", "summary"))
    args = parser.parse_args()
    try:
        config = read("run.json")
        if args.action == "initial":
            check_initial(config)
        elif args.action == "lookup":
            print(json.dumps(check_lookup(config), indent=2))
        else:
            result = check_order(config)
            if args.action == "order-result":
                print(result["order"]["refusal"])
                return 1
            if args.action == "summary":
                Path("evidence.json").write_text(json.dumps(result, indent=2) + "\n")
                print(f"{'Operation':<15} {'Gateway result':<20} Supplier evidence")
                print(f"{'Stock lookup':<15} {'Allowed, HTTP ' + str(result['lookup']['gatewayStatus']):<20} "
                      f"Credential accepted; {result['lookup']['available']} filters available")
                print(f"{'Place order':<15} {'Refused, HTTP ' + str(result['order']['gatewayStatus']):<20} "
                      f"{result['order']['supplierRequests']} order requests; {result['order']['ordersCreated']} orders created")
            else:
                print(json.dumps(result["order"], indent=2))
    except (ValueError, KeyError, TypeError, OSError) as error:
        print(f"Demo evidence failed: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
