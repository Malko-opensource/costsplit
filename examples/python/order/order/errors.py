class NotFound(Exception):
    """The order does not exist."""


class InvalidTransition(Exception):
    """The event is not allowed in the order's current state."""
