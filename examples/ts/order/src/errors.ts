export class NotFoundError extends Error {
  constructor(id: string) {
    super(`order ${id} not found`);
  }
}

export class InvalidTransitionError extends Error {
  constructor(state: string, event: string) {
    super(`invalid transition: ${state} -> ${event}`);
  }
}
