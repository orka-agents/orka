"""Fixed business requests and acceptance checks for the efficiency demo."""

import json

PAYMENTS_DATA = """Summarize this payment export. Repeated event IDs are duplicate deliveries.
Count each event ID once and add its amount once.
Events: [{"id":"p1","amount":12},{"id":"p2","amount":18},{"id":"p1","amount":12}]
Return the unique payment count and total amount."""

INVENTORY_DATA = """A customer requests 24 replacement filters. We have 18 available today.
The next delivery arrives tomorrow. How many can ship today and how many are short?"""

PAYMENTS_CODE = """Debug a concurrency failure in the payments service. Concurrent retries sometimes
charge the same event twice. Review and correct this implementation:

def charge_once(event_id, amount, ledger, lock, charge):
    if event_id in ledger:
        return ledger[event_id]
    receipt = charge(amount)
    with lock:
        ledger[event_id] = receipt
    return receipt

The ledger is a shared dictionary, lock is a threading.Lock supplied by the caller,
and charge(amount) is a supplied callback returning a receipt. Concurrent calls for
the same event must call charge exactly once and return the same receipt. Different
event IDs must charge independently even when amounts match. If charge raises an
exception, a later retry must be allowed to charge. Preserve the function signature.
Return a JSON object with code and explanation. Code must be exactly this one
function, with no imports, helpers, decorators, annotations, loops, or attribute
access. Use dictionary membership and subscripts. The caller supplies all resources.
The function will be tested with overlapping calls, retries, and a failed charge."""

INVENTORY_CODE = """Debug a concurrency failure in the warehouse service. Concurrent orders sometimes
reserve the same filters, and retries subtract stock again. Review this implementation:

def reserve(order_id, requested, state, lock):
    shipped = min(requested, state["available"])
    with lock:
        state["available"] -= shipped
    return {"shipped": shipped, "shortfall": requested - shipped}

state is a shared dictionary with available as a nonnegative integer and orders as
an initially empty dictionary. lock is a threading.Lock supplied by the caller.
Reject negative requested quantities with ValueError. A new order ships as many
filters as are available and returns exactly shipped and shortfall. Save its result
in state["orders"]. Retrying that order ID with the same quantity must return its
original result without subtracting stock again. Concurrent orders must never
oversell. Preserve the function signature. Inputs are otherwise valid integers.
Return a JSON object with code and explanation. Code must be exactly this one
function, with no imports, helpers, decorators, annotations, loops, or attribute
access. Use dictionary membership and subscripts, min or max, and the supplied lock.
The function will be tested with overlapping orders, retries, and sufficient stock."""

WORKLOADS = {
    "payments-summary": {
        "team": "payments", "title": "Count payments once", "prompt": PAYMENTS_DATA,
        "kind": "data", "expected": {"unique_payments": 2, "total_amount": 30},
    },
    "payments-fix": {
        "team": "payments", "title": "Prevent duplicate charges", "prompt": PAYMENTS_CODE,
        "kind": "code", "function": "charge_once",
    },
    "inventory-stock": {
        "team": "inventory", "title": "Check the filter shortage", "prompt": INVENTORY_DATA,
        "kind": "data", "expected": {"available_now": 18, "shortfall": 6},
    },
    "inventory-fix": {
        "team": "inventory", "title": "Prevent overselling", "prompt": INVENTORY_CODE,
        "kind": "code", "function": "reserve",
    },
}

CODE_FORMAT = ('For a debugging request use {"code": "<function>", '
               '"explanation": "<brief reason>"}.')


def agent_spec(team, structured=True):
    if team == "payments":
        rule = ('Return only a JSON object. Never call tools. For a payment export use this '
                'exact shape: {"unique_payments": <integer count>, "total_amount": <integer sum>}. '
                'Count each event ID once.')
    elif structured:
        rule = ('Return only a JSON object. Never call tools. For a stock question use this '
                'exact shape: {"available_now": <integer>, "shortfall": <integer>, '
                '"next_action": "Offer <available_now> today and the remaining <shortfall> tomorrow."}. '
                'Replace the placeholders with integers computed from the supplied stock and request.')
    else:
        rule = ("Never call tools. For stock questions, answer only with this plain sentence, replacing the braces "
                "with the computed integer values: We can ship {available_now} today, "
                "with {shortfall} remaining until tomorrow.")
    return {
        "providerRef": {"name": "platform"},
        "model": {"name": "team-assistant", "maxTokens": 1536},
        "systemPrompt": {"inline": rule + " " + CODE_FORMAT},
        "tools": [],
        "resources": {"requests": {"cpu": "100m", "memory": "256Mi"},
                      "limits": {"cpu": "1", "memory": "512Mi"}},
    }


def client_request(workload):
    """The same bytes are submitted before and after platform changes."""
    item = WORKLOADS[workload]
    agent = "efficiency-" + item["team"]
    return {
        "model": "platform/coordinator",
        "messages": [{"role": "user", "content": (
            f"Use the existing native AI Agent named {agent} for this request. "
            "Create exactly one AI Task with create_ai_task and agentRef set to that Agent. "
            "Omit providerRef so the Agent's configured provider and model are used. "
            "Do not create an Agent or use a CLI runtime. The JSON object below supplies task_prompt. "
            "Use exactly that decoded string as the Task prompt, without headings, field names, "
            "quotation marks, or added instructions. Preserve its whitespace and punctuation. "
            "Wait for that Task and retrieve its result. "
            "Do not answer the request yourself. Return the Task's answer verbatim, "
            "without adding a preamble.\n\n" + json.dumps({"task_prompt": item["prompt"]})
        )}],
        "max_tokens": 4096,
        "stream": False,
    }
