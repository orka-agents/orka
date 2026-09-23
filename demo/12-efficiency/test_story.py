"""Checks for the fixed support workload and its acceptance boundary."""

import unittest

import evidence
import story


class SupportReplies(unittest.TestCase):
    def test_twenty_distinct_matched_requests(self):
        self.assertEqual(len(story.CUSTOMERS), 20)
        self.assertEqual(len(set(story.PROMPTS.values())), 21)
        self.assertEqual(sum(bool(row["orderReference"]) for row in story.CUSTOMERS), 10)

    def test_missing_reference_is_requested(self):
        answer = "I'm sorry you saw two charges after checkout froze. Could you share your order reference?"
        self.assertEqual(len(story.support_checks(answer, "support-01")), 4)
        # A conditional purpose is neither a promise nor a completed action.
        answer = "We acknowledge your report of two charges appearing after a checkout freeze. Please provide your order reference so we can investigate the discrepancy."
        self.assertEqual(len(story.support_checks(answer, "support-01")), 4)

    def test_supplied_reference_is_used_without_repeating_the_question(self):
        for acknowledgement in (
                "Thank you for sharing the order reference.",
                "Thank you for sending your order reference.",
                "Thank you, you provided the order reference."):
            with self.subTest(acknowledgement=acknowledgement):
                answer = "I'm sorry you saw duplicate charges for order ORD-4102. " + acknowledgement
                self.assertEqual(len(story.support_checks(answer, "support-02")), 4)
        with self.assertRaisesRegex(evidence.EvidenceError, "already had"):
            story.support_checks("I'm sorry about duplicate charges for ORD-4102. Please send your order reference.", "support-02")
        with self.assertRaisesRegex(evidence.EvidenceError, "lost the supplied"):
            story.support_checks("I'm sorry you saw duplicate charges. Thank you for the details.", "support-02")
        for reference in ("ORD-41020", "ORD-4102A", "ORD-4102-extra", "ORD-4102 and ORD-4104"):
            with self.subTest(reference=reference), self.assertRaisesRegex(evidence.EvidenceError, "different order"):
                story.support_checks(f"I'm sorry you saw duplicate charges for {reference}. Thank you for the details.", "support-02")

    def test_rejects_invented_reference_and_completed_refund(self):
        for answer in (
                "I'm sorry for the duplicate charges on ORD-9999. Please share the order reference.",
                "We have already refunded the duplicate charge. Please share your order reference.",
                "We will investigate the duplicate charge. Please share your order reference.",
                "We have reviewed your duplicate charges. Please share your order reference.",
                "I see duplicate charges but cannot find the order reference you provided. Please share your order reference."):
            with self.subTest(answer=answer), self.assertRaises(evidence.EvidenceError):
                story.support_checks(answer, "support-01")

    def test_rejects_refund_promise_after_supplied_reference(self):
        for answer in (
                "I'm sorry about the duplicate charges for order ORD-4102. Your refund will arrive tomorrow.",
                "I'm sorry about duplicate charges for ORD-4102. Your refund arrives tomorrow.",
                "The payment page stopped responding, and you have reported two charges for order ORD-4102. I will investigate this matter and provide you with a refund or investigation as soon as possible."):
            with self.subTest(answer=answer), self.assertRaisesRegex(evidence.EvidenceError, "action not established"):
                story.support_checks(answer, "support-02")

    def test_rejects_ordinary_investigation_and_future_action_promises(self):
        for promise in (
                "We'll look into this for you.",
                "We\u2019ll investigate this for you.",
                "I'll check your account.",
                "I\u2019ll look into this.",
                "Our team will review your charges.",
                "We're going to check this for you.",
                "Your charges will be checked."):
            with self.subTest(promise=promise), self.assertRaisesRegex(evidence.EvidenceError, "action not established"):
                story.support_checks("I'm sorry you saw duplicate charges for order ORD-4102. " + promise, "support-02")

    def test_rejects_completed_and_ongoing_account_action_claims(self):
        for claim in (
                "we checked your account",
                "we have checked your account",
                "we\u2019ve reviewed your account",
                "I'm checking your account",
                "we are looking into the problem",
                "our team has reviewed the charges"):
            with self.subTest(claim=claim), self.assertRaisesRegex(evidence.EvidenceError, "account action"):
                story.support_checks("I'm sorry about duplicate charges for order ORD-4102; " + claim
                                     + ". Thank you for sharing the details.", "support-02")


    def test_rejects_generic_or_overlong_answers(self):
        for answer in (
                "Thanks for contacting us. Please share your order reference.",
                "I'm sorry you saw duplicate charges. Please share your order reference. Thanks."):
            with self.subTest(answer=answer), self.assertRaises(evidence.EvidenceError):
                story.support_checks(answer, "support-01")


class EngineeringPublication(unittest.TestCase):
    def test_only_the_tasks_verified_commit_can_be_the_demonstrated_fix(self):
        publication = {"branch": "demos/run", "commit": "a" * 40, "parent": story.REVISION}
        task = {"spec": {"workspace": {"pushBranch": "demos/run"}}, "status": {"delivery": {
            "state": "VerifiedExact", "expectedCommitSHA": "a" * 40, "verifiedRemoteSHA": "a" * 40,
            "startingSHA": story.REVISION, "branch": "demos/run"}}}
        story.publication_checks(task, publication)
        publication["commit"] = "b" * 40
        with self.assertRaisesRegex(evidence.EvidenceError, "verified delivery"):
            story.publication_checks(task, publication)



if __name__ == "__main__":
    unittest.main()
