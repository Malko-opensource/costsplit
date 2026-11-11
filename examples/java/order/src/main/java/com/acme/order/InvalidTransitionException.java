package com.acme.order;

public class InvalidTransitionException extends RuntimeException {
    private static final long serialVersionUID = 1L;

    public InvalidTransitionException(State state, Event event) {
        super("invalid transition: " + state + " -> " + event);
    }
}
