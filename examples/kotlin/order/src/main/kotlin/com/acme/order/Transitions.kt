package com.acme.order

/** Read directly by the harness and the tests. */
val TRANSITIONS: Map<State, Map<Event, State>> = mapOf(
    State.PENDING to mapOf(Event.PAY to State.PAID, Event.CANCEL to State.CANCELLED),
    State.PAID to mapOf(Event.SHIP to State.SHIPPED, Event.CANCEL to State.CANCELLED),
)
