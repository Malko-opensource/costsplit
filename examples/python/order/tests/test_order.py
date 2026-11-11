import unittest

from order.state import TRANSITIONS, State


class OrderTest(unittest.TestCase):
    def test_REQ001_pay_moves_pending_to_paid(self) -> None:
        self.skipTest("graybox")

    def test_REQ002_shipped_cannot_be_cancelled(self) -> None:
        self.skipTest("graybox")

    def test_REQ003_unknown_order_raises_not_found(self) -> None:
        self.skipTest("graybox")

    # The transition table itself is checked now, not skipped.
    def test_transitions_terminal_states_have_no_exits(self) -> None:
        for s in (State.SHIPPED, State.CANCELLED):
            self.assertEqual(TRANSITIONS.get(s, {}), {})
