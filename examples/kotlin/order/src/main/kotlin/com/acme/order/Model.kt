package com.acme.order

enum class State { PENDING, PAID, SHIPPED, CANCELLED }

enum class Event { PAY, SHIP, CANCEL }

/** The aggregate the service changes. */
data class Order(val id: String, val state: State)

class NotFoundException(id: String) : RuntimeException("order $id not found")

class InvalidTransitionException(state: State, event: Event) :
    RuntimeException("invalid transition: $state -> $event")
