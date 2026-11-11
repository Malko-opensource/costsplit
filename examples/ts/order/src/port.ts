import type { Order } from "./model.ts";

export interface Repository {
  get(id: string): Promise<Order>;
  save(order: Order): Promise<void>;
}
