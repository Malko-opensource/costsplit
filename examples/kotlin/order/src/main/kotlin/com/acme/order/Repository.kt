package com.acme.order

/** The storage port. */
interface Repository {
    fun get(id: String): Order

    fun save(order: Order)
}
