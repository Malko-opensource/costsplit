from order.errors import InvalidTransition
from order.model import Order
from order.port import Repository
from order.state import TRANSITIONS, Event


class Service:
    def __init__(self, repo: Repository) -> None:
        self._repo = repo

    def apply(self, order_id: str, event: Event) -> Order:
        """Load the order, apply the event and save the result."""
        raise NotImplementedError
