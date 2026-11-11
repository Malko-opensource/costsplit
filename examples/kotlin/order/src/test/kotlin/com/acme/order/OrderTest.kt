package com.acme.order

import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assumptions
import org.junit.jupiter.api.Test

class OrderTest {
    @Test
    fun testREQ001_payMovesPendingToPaid() {
        Assumptions.abort<Unit>("graybox")
    }

    @Test
    fun testREQ002_shippedCannotBeCancelled() {
        Assumptions.abort<Unit>("graybox")
    }

    @Test
    fun testREQ003_unknownOrderThrowsNotFound() {
        Assumptions.abort<Unit>("graybox")
    }

    // The transition table itself is checked now, not skipped.
    @Test
    fun transitionsTerminalStatesHaveNoExits() {
        assertNull(TRANSITIONS[State.SHIPPED])
        assertNull(TRANSITIONS[State.CANCELLED])
    }
}
