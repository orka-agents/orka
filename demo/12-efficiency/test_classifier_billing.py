import copy
import unittest

import classifier_billing as billing


def zero_receipt():
    identity = "gen_" + "A" * 26
    failure = {"generationID": identity, "statusCode": 503, "reportedUsage": False, "operationID": "work-1"}
    data = {"id": identity, "model": "typesafe-ai/jev", "provider_name": "typesafe-ai",
            "is_byok": False, "finish_reason": "error",
            **dict.fromkeys((*billing.MONEY_FIELDS, *billing.TOKEN_FIELDS), 0)}
    return failure, {"id": identity, "url": billing.generation_url(identity), "httpStatus": 200,
                     "response": {"data": data}}


class ClassifierBillingTests(unittest.TestCase):
    def test_exact_failed_generation_proves_zero_cost_separately_from_usage(self):
        failure, receipt = zero_receipt()
        original = copy.deepcopy(failure)
        self.assertEqual(billing.verify_receipts([failure], [receipt]), [
            {"generationID": failure["generationID"], "marketCostUsd": "0", "gatewayCostUsd": "0"}])
        self.assertEqual(failure, original)
        self.assertIs(failure["reportedUsage"], False)

    def test_zero_promotional_debit_cannot_replace_nonzero_market_cost(self):
        failure, receipt = zero_receipt()
        receipt["response"]["data"]["market_cost"] = 0.0002
        with self.assertRaises(billing.BillingError):
            billing.verify_zero_charge(failure, receipt)

    def test_every_cost_and_token_counter_is_required_and_explicitly_zero(self):
        for field in (*billing.MONEY_FIELDS, *billing.TOKEN_FIELDS):
            for invalid in (None, 1, True, float("nan"), float("inf")):
                failure, receipt = zero_receipt()
                receipt["response"]["data"][field] = invalid
                with self.subTest(field=field, invalid=invalid), self.assertRaises(billing.BillingError):
                    billing.verify_zero_charge(failure, receipt)
            failure, receipt = zero_receipt()
            del receipt["response"]["data"][field]
            with self.subTest(field=field), self.assertRaises(billing.BillingError):
                billing.verify_zero_charge(failure, receipt)

    def test_successful_or_unrelated_receipt_cannot_cover_a_failure(self):
        for field, value in (("finish_reason", "stop"), ("id", "gen_" + "B" * 26),
                             ("model", "other"), ("provider_name", "other"), ("is_byok", True)):
            failure, receipt = zero_receipt()
            receipt["response"]["data"][field] = value
            with self.subTest(field=field), self.assertRaises(billing.BillingError):
                billing.verify_zero_charge(failure, receipt)
        for field, value in (("statusCode", 200), ("reportedUsage", True)):
            failure, receipt = zero_receipt()
            failure[field] = value
            with self.subTest(field=field), self.assertRaises(billing.BillingError):
                billing.verify_zero_charge(failure, receipt)

    def test_lookup_identity_and_origin_are_fixed(self):
        for invalid in (None, "", "gen_short", "../../secret", "gen_" + "A" * 26 + "?id=other"):
            with self.subTest(invalid=invalid), self.assertRaises(billing.BillingError):
                billing.generation_url(invalid)
        failure, receipt = zero_receipt()
        receipt["url"] = "https://example.invalid/v1/generation?id=" + failure["generationID"]
        with self.assertRaises(billing.BillingError):
            billing.verify_zero_charge(failure, receipt)

    def test_missing_duplicate_and_extra_receipts_fail(self):
        failure, receipt = zero_receipt()
        for failures, receipts in (([failure], []), ([], [receipt]), ([failure], [receipt, receipt]),
                                   ([failure, failure], [receipt])):
            with self.subTest(failures=len(failures), receipts=len(receipts)), self.assertRaises(billing.BillingError):
                billing.verify_receipts(failures, receipts)


if __name__ == "__main__":
    unittest.main()
