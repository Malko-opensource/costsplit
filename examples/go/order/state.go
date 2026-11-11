package order

// State is an order state.
type State string

// Event moves an order between states.
type Event string

const (
	Pending   State = "pending"
	Paid      State = "paid"
	Shipped   State = "shipped"
	Cancelled State = "cancelled"
)

const (
	Pay    Event = "pay"
	Ship   Event = "ship"
	Cancel Event = "cancel"
)

// Transitions is read directly by the harness and the tests.
var Transitions = map[State]map[Event]State{
	Pending: {Pay: Paid, Cancel: Cancelled},
	Paid:    {Ship: Shipped, Cancel: Cancelled},
}
