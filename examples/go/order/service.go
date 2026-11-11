package order

import "context"

// Service applies events to orders.
type Service struct{ repo Repository }

// NewService builds a Service.
func NewService(repo Repository) *Service { return &Service{repo: repo} }

// Apply loads the order, applies the event and saves the result.
func (s *Service) Apply(ctx context.Context, id OrderID, e Event) (Order, error) {
	panic("not implemented")
}
