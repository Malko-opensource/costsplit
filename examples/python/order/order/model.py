from dataclasses import dataclass

from order.state import State


@dataclass(frozen=True)
class Order:
    id: str
    state: State
