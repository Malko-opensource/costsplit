export type State = "pending" | "paid" | "shipped" | "cancelled";
export type Event = "pay" | "ship" | "cancel";

// Read directly by the harness and the tests.
export const transitions: Readonly<Partial<Record<State, Partial<Record<Event, State>>>>> = {
  pending: { pay: "paid", cancel: "cancelled" },
  paid: { ship: "shipped", cancel: "cancelled" },
};
