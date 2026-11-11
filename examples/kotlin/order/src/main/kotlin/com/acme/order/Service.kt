package com.acme.order

class Service(private val repo: Repository) {
    /** Loads the order, applies the event and saves the result. */
    fun apply(id: String, event: Event): Order {
        TODO()
    }
}
