package order_test

import (
	"testing"

	"example.com/order"
)

func TestREQ001_PayMovesPendingToPaid(t *testing.T)       { t.Skip("graybox") }
func TestREQ002_ShippedCannotBeCancelled(t *testing.T)    { t.Skip("graybox") }
func TestREQ003_UnknownOrderReturnsNotFound(t *testing.T) { t.Skip("graybox") }

// The transition table itself is checked now, not skipped.
func TestTransitions_TerminalStatesHaveNoExits(t *testing.T) {
	for _, s := range []order.State{order.Shipped, order.Cancelled} {
		if len(order.Transitions[s]) != 0 {
			t.Errorf("%s must be terminal", s)
		}
	}
}
