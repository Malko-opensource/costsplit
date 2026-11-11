import type { Order } from "./model.ts";
import type { Repository } from "./port.ts";
import { transitions, type Event } from "./state.ts";
import { InvalidTransitionError } from "./errors.ts";

export class Service {
  private readonly repo: Repository;

  constructor(repo: Repository) {
    this.repo = repo;
  }

  /** Load the order, apply the event and save the result. */
  async apply(id: string, event: Event): Promise<Order> {
    throw new Error("not implemented");
  }
}
