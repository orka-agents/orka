"""The cross-team bridge must not treat arbitrary chat as fix authorization."""

import unittest
from workflow import eligible_event, FIX_REQUEST, GUIDE


class AuthorizationTest(unittest.TestCase):
    def setUp(self):
        self.binding = {"metadata": {"uid": "binding-1"}, "spec": {"match": {
            "senderId": "allowed-person", "contextId": "demo-conversation", "accountId": "tenant-1"}}}
        self.event = {"text": FIX_REQUEST, "gatewayName": "teams", "agentName": GUIDE,
            "bindingUid": "binding-1", "senderId": "allowed-person", "contextId": "demo-conversation",
            "accountId": "tenant-1", "state": "TaskCreated", "taskName": "message-task", "taskUid": "task-1"}

    def test_exact_authorized_message(self):
        self.assertTrue(eligible_event(self.event, self.binding))

    def test_unrelated_or_unadmitted_messages_do_not_start_engineering(self):
        for key, value in {
            "text": "What needs attention?", "gatewayName": "telegram", "agentName": "another-agent",
            "bindingUid": "old-binding", "senderId": "another-person", "contextId": "another-conversation",
            "accountId": "another-tenant", "state": "Rejected", "taskName": "", "taskUid": "",
        }.items():
            with self.subTest(key=key):
                self.assertFalse(eligible_event({**self.event, key: value}, self.binding))


if __name__ == "__main__":
    unittest.main()
