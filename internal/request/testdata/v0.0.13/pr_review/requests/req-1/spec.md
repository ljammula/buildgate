# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. A retried POST /refunds with the same idempotency key returns the original result.
2. A non-idempotent POST /refunds still processes normally.

## Risks

None known.

## Open questions

None.
