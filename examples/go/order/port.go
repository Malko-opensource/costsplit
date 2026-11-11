package order

import "context"

// Repository is the storage port.
type Repository interface {
	Get(ctx context.Context, id OrderID) (Order, error)
	Save(ctx context.Context, o Order) error
}
