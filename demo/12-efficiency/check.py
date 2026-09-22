#!/usr/bin/env python3
"""Validate the fixed business examples; never accept an agent's success claim."""

import argparse
import ast
from concurrent.futures import ThreadPoolExecutor
import json
import sys
import threading
import time


def require(value, message):
    if not value:
        raise ValueError(message)


def load_function(code, name):
    # The fixture needs only bounded arithmetic, dictionary access, and one
    # caller-owned lock. Reject imports, attribute access, loops and arbitrary
    # calls before executing model-generated code in the validation container.
    require(isinstance(code, str) and len(code) <= 6000, "invalid code size")
    tree = ast.parse(code)
    allowed = {
        ast.Module, ast.FunctionDef, ast.arguments, ast.arg, ast.Return, ast.If,
        ast.IfExp, ast.Compare, ast.Gt, ast.GtE, ast.Lt, ast.LtE, ast.Eq, ast.NotEq,
        ast.In, ast.NotIn, ast.Is, ast.IsNot, ast.BinOp, ast.Add, ast.Sub,
        ast.Constant, ast.Name, ast.Load, ast.Store, ast.Call, ast.Expr,
        ast.UnaryOp, ast.USub, ast.Not, ast.Assign, ast.AugAssign, ast.Subscript,
        ast.Dict, ast.With, ast.withitem, ast.Raise, ast.BoolOp, ast.And, ast.Or,
    }
    require(all(type(n) in allowed for n in ast.walk(tree)), "code exceeds the fixture's allowed operations")
    require(len(tree.body) == 1 and isinstance(tree.body[0], ast.FunctionDef), "expected one function")
    fn = tree.body[0]
    require(fn.name == name and not fn.decorator_list and not fn.returns, "unexpected function")
    require(sum(isinstance(n, ast.FunctionDef) for n in ast.walk(tree)) == 1, "nested function")
    expected = (["event_id", "amount", "ledger", "lock", "charge"] if name == "charge_once"
                else ["order_id", "requested", "state", "lock"])
    require([a.arg for a in fn.args.args] == expected, "function signature changed")
    require(not fn.args.defaults and not fn.args.kwonlyargs and not fn.args.vararg
            and not fn.args.kwarg and not fn.args.posonlyargs, "unexpected argument declarations")
    require(all(not a.annotation for a in fn.args.args), "annotations are not needed")
    for node in ast.walk(tree):
        if isinstance(node, ast.Name):
            require(not node.id.startswith("_") and len(node.id) <= 64, "unexpected identifier")
        if isinstance(node, ast.Call):
            require(isinstance(node.func, ast.Name)
                    and node.func.id in {"charge", "min", "max", "ValueError"}, "unexpected function call")
    env = {"__builtins__": {}, "min": min, "max": max, "ValueError": ValueError}
    exec(compile(tree, "<proposed-fix>", "exec"), env)
    return env[name]


def payment_checks(fn):
    checks = []
    ledger, lock, charges = {}, threading.Lock(), []

    def charge(amount):
        # Make overlap observable even on a small CPU node.
        time.sleep(0.01)
        charges.append(amount)
        return "receipt-" + str(len(charges))

    with ThreadPoolExecutor(max_workers=8) as pool:
        replies = list(pool.map(lambda _: fn("event-1", 30, ledger, lock, charge), range(16)))
    require(charges == [30], "concurrent retries charged more than once")
    checks.append("16 overlapping retries created one charge")
    require(replies == ["receipt-1"] * 16, "retries returned different receipts")
    checks.append("all retries returned the original receipt")
    require(fn("event-1", 30, ledger, lock, charge) == "receipt-1" and charges == [30],
            "later retry charged again")
    checks.append("a later retry did not charge again")
    require(fn("event-2", 30, ledger, lock, charge) == "receipt-2" and charges == [30, 30],
            "different event with same amount was lost")
    checks.append("a different event with the same amount was charged")

    def failure(_):
        raise RuntimeError("synthetic supplier failure")

    try:
        fn("event-3", 15, ledger, lock, failure)
    except RuntimeError:
        pass
    else:
        raise ValueError("charge error was swallowed")
    require("event-3" not in ledger, "failed charge was saved as complete")
    checks.append("a failed charge was not saved as complete")
    require(fn("event-3", 15, ledger, lock, charge) == "receipt-3", "failed charge could not retry")
    checks.append("a failed charge could be retried")
    return checks


def overlapping_reservations(fn, order_ids):
    """Hold unprotected reads until eight calls have observed the same state."""
    active = threading.local()
    barriers, guard = {}, threading.Lock()

    class SharedLock:
        def __init__(self):
            self.mutex = threading.Lock()
            self.owner = None

        def __enter__(self):
            self.mutex.acquire()
            self.owner = threading.get_ident()
            return self

        def __exit__(self, *args):
            self.owner = None
            self.mutex.release()

    lock = SharedLock()

    def interleave(point):
        if not getattr(active, "enabled", False) or lock.owner == threading.get_ident():
            return
        with guard:
            barrier = barriers.setdefault(point, threading.Barrier(8))
        try:
            barrier.wait(timeout=5)
        except threading.BrokenBarrierError as exc:
            raise ValueError("overlapping stock access did not synchronize") from exc

    class Orders(dict):
        def __contains__(self, key):
            present = super().__contains__(key)
            interleave("order membership")
            return present

    class Stock(dict):
        def __getitem__(self, key):
            value = super().__getitem__(key)
            if key == "available":
                interleave("available stock")
            return value

    state = Stock(available=18, orders=Orders())

    def reserve(order_id):
        active.enabled = True
        try:
            interleave("start")
            return fn(order_id, 12, state, lock)
        finally:
            active.enabled = False

    with ThreadPoolExecutor(max_workers=8) as pool:
        replies = list(pool.map(reserve, order_ids))
    return state, replies


def inventory_checks(fn):
    checks = []
    lock = threading.Lock()
    state = {"available": 18, "orders": {}}
    first = fn("order-1", 24, state, lock)
    require(first == {"shipped": 18, "shortfall": 6} and state["available"] == 0, "wrong shortage")
    checks.append("18 available against 24 requested leaves a shortage of 6")
    require(fn("order-1", 24, state, lock) == first and state["available"] == 0, "retry changed stock")
    checks.append("retry returned the original allocation without changing stock")
    state = {"available": 24, "orders": {}}
    require(fn("order-2", 18, state, lock) == {"shipped": 18, "shortfall": 0}
            and state["available"] == 6, "sufficient stock handled incorrectly")
    checks.append("sufficient stock produces zero shortage")
    state, replies = overlapping_reservations(fn, ["parallel-" + str(i) for i in range(8)])
    require(sum(r["shipped"] for r in replies) == 18 and state["available"] == 0,
            "concurrent orders oversold or lost stock")
    checks.append("eight overlapping orders shared exactly 18 filters")
    require(len(state["orders"]) == 8, "orders were not retained")
    checks.append("each allocation was saved for retries")
    state, replies = overlapping_reservations(fn, ["same-order"] * 16)
    require(replies == [{"shipped": 12, "shortfall": 0}] * 16 and state["available"] == 6,
            "overlapping retries changed the allocation")
    checks.append("16 overlapping retries allocated stock once")
    before = {"available": state["available"], "orders": dict(state["orders"])}
    try:
        fn("negative", -1, state, lock)
    except ValueError:
        pass
    else:
        raise ValueError("negative quantity was accepted")
    require(state == before, "invalid request changed stock")
    checks.append("negative quantities were rejected without changing stock")
    require(fn("zero", 0, state, lock) == {"shipped": 0, "shortfall": 0}, "zero quantity failed")
    checks.append("zero quantity produces a zero allocation")
    return checks


def check_stock_prose(answer):
    available, requested = 18, 24
    expected = f"We can ship {available} today, with {requested - available} remaining until tomorrow."
    require(isinstance(answer, str) and answer.strip() == expected,
            "stock prose does not match the checked quantities and timing")
    return ["stock and shortage in prose"]


def check_data(workload, answer):
    expected = {"unique_payments": 2, "total_amount": 30} if workload == "payments-summary" else {
        "available_now": 18, "shortfall": 6}
    require(isinstance(answer, dict), "answer must be a JSON object")
    for key, value in expected.items():
        require(type(answer.get(key)) is int and answer[key] == value, "incorrect " + key)
    if workload == "inventory-stock":
        action = answer.get("next_action")
        expected_action = f"Offer {answer['available_now']} today and the remaining {answer['shortfall']} tomorrow."
        require(isinstance(action, str) and action.strip() == expected_action,
                "customer next action does not match the checked quantities and timing")
    return list(expected) + (["customer next action"] if workload == "inventory-stock" else [])


def check_answer(workload, answer):
    if workload.endswith("-fix"):
        require(isinstance(answer, dict) and isinstance(answer.get("explanation"), str)
                and answer["explanation"].strip(), "missing explanation")
        name = "charge_once" if workload == "payments-fix" else "reserve"
        fn = load_function(answer.get("code"), name)
        checks = payment_checks(fn) if name == "charge_once" else inventory_checks(fn)
    else:
        checks = check_data(workload, answer)
    return {"workload": workload, "passed": True, "checks": checks, "checkCount": len(checks)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("workload")
    parser.add_argument("answer", help="JSON answer, or - to read stdin")
    args = parser.parse_args()
    try:
        answer = json.loads(sys.stdin.read() if args.answer == "-" else args.answer)
        print(json.dumps(check_answer(args.workload, answer)))
    except (ValueError, KeyError, TypeError, SyntaxError, AssertionError) as exc:
        print(json.dumps({"workload": args.workload, "passed": False, "error": str(exc)}))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
