package com.acme.order;

public class Service {
    private final Repository repo;

    public Service(Repository repo) {
        this.repo = repo;
    }

    /** Loads the order, applies the event and saves the result. */
    public Order apply(String id, Event event) {
        throw new UnsupportedOperationException("not implemented");
    }
}
