package com.acme.order;

public class NotFoundException extends RuntimeException {
    private static final long serialVersionUID = 1L;

    public NotFoundException(String id) {
        super("order " + id + " not found");
    }
}
