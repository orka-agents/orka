# Checkout retry example

A customer clicked Pay again while checkout was waiting for the payment service.
The two overlapping requests used the same event ID but created two charges.

Run the regression checks with Node 22 or newer:

```sh
node --test payments/payment.test.mjs
```

The starting implementation deliberately fails two checks. Fix `charge.mjs`
without changing the tests. This small example uses an in-memory ledger and a
synthetic payment callback. It does not place real charges or demonstrate
cross-process payment deduplication.
