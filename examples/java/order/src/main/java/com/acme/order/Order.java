package com.acme.order;

/** The aggregate the service changes. */
public record Order(String id, State state) {}
