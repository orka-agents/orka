"""Exercise failures that would otherwise make the recorded evidence misleading."""

import unittest

from check import check_answer, check_stock_prose, load_function
from fixtures import client_request


PAYMENTS = '''def charge_once(event_id, amount, ledger, lock, charge):
    with lock:
        if event_id in ledger:
            return ledger[event_id]
        receipt = charge(amount)
        ledger[event_id] = receipt
        return receipt
'''

INVENTORY = '''def reserve(order_id, requested, state, lock):
    if requested < 0:
        raise ValueError("negative quantity")
    with lock:
        if order_id in state["orders"]:
            return state["orders"][order_id]
        shipped = min(requested, state["available"])
        state["available"] -= shipped
        result = {"shipped": shipped, "shortfall": requested - shipped}
        state["orders"][order_id] = result
        return result
'''

UNLOCKED_INVENTORY = '''def reserve(order_id, requested, state, lock):
    if requested < 0:
        raise ValueError("negative quantity")
    if order_id in state["orders"]:
        return state["orders"][order_id]
    shipped = min(requested, state["available"])
    state["available"] -= shipped
    result = {"shipped": shipped, "shortfall": requested - shipped}
    state["orders"][order_id] = result
    return result
'''

READ_BEFORE_LOCK_INVENTORY = '''def reserve(order_id, requested, state, lock):
    if requested < 0:
        raise ValueError("negative quantity")
    available = state["available"]
    with lock:
        if order_id in state["orders"]:
            return state["orders"][order_id]
        shipped = min(requested, available)
        state["available"] -= shipped
        result = {"shipped": shipped, "shortfall": requested - shipped}
        state["orders"][order_id] = result
        return result
'''

MEMBERSHIP_BEFORE_LOCK_INVENTORY = '''def reserve(order_id, requested, state, lock):
    if requested < 0:
        raise ValueError("negative quantity")
    if order_id in state["orders"]:
        return state["orders"][order_id]
    with lock:
        shipped = min(requested, state["available"])
        state["available"] -= shipped
        result = {"shipped": shipped, "shortfall": requested - shipped}
        state["orders"][order_id] = result
        return result
'''


class EvidenceChecks(unittest.TestCase):
    def test_duplicate_payments_are_not_counted(self):
        result = check_answer("payments-summary", {"unique_payments": 2, "total_amount": 30})
        self.assertTrue(result["passed"])
        with self.assertRaises(ValueError):
            check_answer("payments-summary", {"unique_payments": 3, "total_amount": 42})

    def test_stock_requires_the_new_customer_action(self):
        with self.assertRaises(ValueError):
            check_answer("inventory-stock", {"available_now": 18, "shortfall": 6})
        self.assertTrue(check_answer("inventory-stock", {
            "available_now": 18, "shortfall": 6,
            "next_action": " \nOffer 18 today and the remaining 6 tomorrow.\t",
        })["passed"])

    def test_customer_action_rejects_contradictory_quantities_and_timing(self):
        for action in (
            "Only 6 can ship today; the other 18 arrive tomorrow.",
            "Offer 6 today and the remaining 18 tomorrow.",
            "Offer 18 today and the remaining 6 tomorrow. Actually, nothing ships today.",
            "Offer 18 tomorrow and the remaining 6 today.",
            "Offer 18 today and the remaining 6 next week.",
            "Offer 18  today and the remaining 6 tomorrow.",
        ):
            with self.subTest(action=action), self.assertRaisesRegex(ValueError, "customer next action"):
                check_answer("inventory-stock", {"available_now": 18, "shortfall": 6, "next_action": action})

    def test_plain_stock_sentence_accepts_only_correct_quantities_and_timing(self):
        self.assertEqual(check_stock_prose(" \nWe can ship 18 today, with 6 remaining until tomorrow.\t"),
                         ["stock and shortage in prose"])
        for answer in (
            "Ship 6 today; 18 are short.",
            "We can ship 6 today, with 18 remaining until tomorrow.",
            "We can ship 18 today, with 6 remaining until tomorrow. Actually, nothing ships today.",
            "We can ship 18 tomorrow, with 6 remaining until today.",
            "We can ship 18  today, with 6 remaining until tomorrow.",
        ):
            with self.subTest(answer=answer), self.assertRaisesRegex(ValueError, "stock prose"):
                check_stock_prose(answer)

    def test_correct_fixes_pass_business_cases(self):
        self.assertEqual(check_answer("payments-fix", {"code": PAYMENTS, "explanation": "Lock the charge."})["checkCount"], 6)
        self.assertEqual(check_answer("inventory-fix", {"code": INVENTORY, "explanation": "Lock the allocation."})["checkCount"], 8)

    def test_original_payment_race_fails(self):
        bad = '''def charge_once(event_id, amount, ledger, lock, charge):
    if event_id in ledger:
        return ledger[event_id]
    receipt = charge(amount)
    with lock:
        ledger[event_id] = receipt
    return receipt
'''
        with self.assertRaisesRegex(ValueError, "charged more than once"):
            check_answer("payments-fix", {"code": bad, "explanation": "Still broken."})

    def test_stock_retry_bug_fails(self):
        bad = INVENTORY.replace('        if order_id in state["orders"]:\n            return state["orders"][order_id]\n', "")
        with self.assertRaisesRegex(ValueError, "retry changed stock"):
            check_answer("inventory-fix", {"code": bad, "explanation": "Still broken."})

    def test_unlocked_and_stale_stock_reads_oversell_under_controlled_overlap(self):
        for code in (UNLOCKED_INVENTORY, READ_BEFORE_LOCK_INVENTORY):
            with self.subTest(code=code), self.assertRaisesRegex(ValueError, "concurrent orders oversold"):
                check_answer("inventory-fix", {"code": code, "explanation": "Still broken."})

    def test_membership_checked_before_lock_allocates_retries_twice(self):
        with self.assertRaisesRegex(ValueError, "overlapping retries changed"):
            check_answer("inventory-fix", {"code": MEMBERSHIP_BEFORE_LOCK_INVENTORY, "explanation": "Still broken."})

    def test_model_code_cannot_import_or_call_unrelated_functions(self):
        for code in [PAYMENTS + '\nimport os\n', PAYMENTS.replace('charge(amount)', 'open("/etc/passwd")'),
                     PAYMENTS.replace('ledger[event_id]', 'ledger.get(event_id)')]:
            with self.assertRaises((ValueError, SyntaxError)):
                load_function(code, "charge_once")

    def test_client_payload_does_not_depend_on_phase_or_destination(self):
        request = client_request("inventory-stock")
        self.assertEqual(request, client_request("inventory-stock"))
        self.assertEqual(request["model"], "platform/coordinator")
        self.assertNotIn("qwen", str(request).lower())
        self.assertNotIn("jev", str(request).lower())


if __name__ == "__main__":
    unittest.main()
