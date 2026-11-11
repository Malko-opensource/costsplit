from order.errors import NotFound
from order.model import Order


class FakeRepo:
    def __init__(self) -> None:
        self.orders: dict[str, Order] = {}

    def get(self, order_id: str) -> Order:
        if order_id not in self.orders:
            raise NotFound(order_id)
        return self.orders[order_id]

    def save(self, order: Order) -> None:
        self.orders[order.id] = order
