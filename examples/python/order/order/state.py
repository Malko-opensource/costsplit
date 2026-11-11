from enum import Enum


class State(str, Enum):
    PENDING = "pending"
    PAID = "paid"
    SHIPPED = "shipped"
    CANCELLED = "cancelled"


class Event(str, Enum):
    PAY = "pay"
    SHIP = "ship"
    CANCEL = "cancel"


# Read directly by the harness and the tests.
TRANSITIONS: dict[State, dict[Event, State]] = {
    State.PENDING: {Event.PAY: State.PAID, Event.CANCEL: State.CANCELLED},
    State.PAID: {Event.SHIP: State.SHIPPED, Event.CANCEL: State.CANCELLED},
}
