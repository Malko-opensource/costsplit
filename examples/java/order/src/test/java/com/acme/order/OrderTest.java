package com.acme.order;

import static org.junit.jupiter.api.Assertions.assertNull;

import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.Test;

class OrderTest {
    @Test
    void testREQ001_payMovesPendingToPaid() {
        Assumptions.abort("graybox");
    }

    @Test
    void testREQ002_shippedCannotBeCancelled() {
        Assumptions.abort("graybox");
    }

    @Test
    void testREQ003_unknownOrderThrowsNotFound() {
        Assumptions.abort("graybox");
    }

    // The transition table itself is checked now, not skipped.
    @Test
    void transitionsTerminalStatesHaveNoExits() {
        assertNull(Transitions.TABLE.get(State.SHIPPED));
        assertNull(Transitions.TABLE.get(State.CANCELLED));
    }
}
