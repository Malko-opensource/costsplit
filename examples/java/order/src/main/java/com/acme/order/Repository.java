package com.acme.order;

/** The storage port. */
public interface Repository {
    Order get(String id);

    void save(Order order);
}
