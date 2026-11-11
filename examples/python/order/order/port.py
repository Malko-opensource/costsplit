from typing import Protocol

from order.model import Order


class Repository(Protocol):
    def get(self, order_id: str) -> Order: ...

    def save(self, order: Order) -> None: ...
