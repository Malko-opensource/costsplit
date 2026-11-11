package com.acme.order;

import java.util.Map;

/** Read directly by the harness and the tests. */
public final class Transitions {
    private Transitions() {}

    public static final Map<State, Map<Event, State>> TABLE = Map.of(
            State.PENDING, Map.of(Event.PAY, State.PAID, Event.CANCEL, State.CANCELLED),
            State.PAID, Map.of(Event.SHIP, State.SHIPPED, Event.CANCEL, State.CANCELLED));
}
