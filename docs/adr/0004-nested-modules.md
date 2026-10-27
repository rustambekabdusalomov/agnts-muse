# 4. Keep the core dependency-free; adapters are nested modules

Status: accepted · Oct 27, 2026

## Context

The engine, protocol client, ledger interfaces and policies need only the standard library. A PostgreSQL store needs pgx, and later stores might need other drivers.

## Decision

The root module imports only the standard library. `ledger/pgledger` is its own module (with a `replace` to the root while both develop) and passes the same `ledgertest` conformance suite as `memledger`.

## Consequences

- Importing `agnts-muse` pulls in no database driver, and a security review of the core covers one module.
- `pgledger` is versioned separately. A host can implement `budget.Store` on its own database and prove it correct with `ledgertest.Run`.
