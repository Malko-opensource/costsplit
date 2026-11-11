package fake

import (
	"context"

	"example.com/order"
)

// Repo is an in-memory Repository.
type Repo struct{ Orders map[order.OrderID]order.Order }

var _ order.Repository = (*Repo)(nil)

func (r *Repo) Get(ctx context.Context, id order.OrderID) (order.Order, error) {
	o, ok := r.Orders[id]
	if !ok {
		return order.Order{}, order.ErrNotFound
	}
	return o, nil
}

func (r *Repo) Save(ctx context.Context, o order.Order) error {
	r.Orders[o.ID] = o
	return nil
}
