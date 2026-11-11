import type { State } from "./state.ts";

export interface Order {
  readonly id: string;
  readonly state: State;
}
