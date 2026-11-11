package order

import "errors"

// OrderID identifies an order.
type OrderID string

// Order is the aggregate the service changes.
type Order struct {
	ID    OrderID
	State State
}

var (
	ErrNotFound          = errors.New("order: not found")
	ErrInvalidTransition = errors.New("order: invalid transition")
)
