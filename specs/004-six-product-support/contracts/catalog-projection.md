# Contract: Product Catalog and Install Projection

## 1. Sole Authored Inventory

`internal/productcatalog` is the only authored list of supported products.
Every descriptor is data-only and deterministically serializable. Runtime
constructors live in the explicit composition root and product packages, not in
the catalog.

The staged host binary exposes:

```text
agent-sessions catalog --json
```

The output is stable, sorted, versioned JSON containing no filesystem discovery
result or credential:

```json
{
  "schema": "agent-sessions.product-catalog.v1",
  "products": [
    {
      "id": "opencode",
      "label": "OpenCode",
      "support_state": "general",
      "native_executable": "opencode",
      "tested_version": "1.18.25",
      "peer_alias": "opencode-peer",
      "lane_alias": "opencode-peer-lane",
      "capabilities": ["interactive", "lane", "parent"],
      "federation_capabilities": ["opencode-lane"],
      "install_root": "integrations/opencode"
    }
  ]
}
```

Unknown schema versions fail closed.

Compatibility remains additive within `agent-sessions.product-catalog.v1`.
Descriptors without a package manager omit both manager fields and retain their
existing minimum/exact product-version behavior. A package-managed tuple is a
single exact boundary and MUST declare all of:

```json
{
  "policy": "exact",
  "package_manager": "pnpm",
  "package_manager_version": "10.28.1",
  "tuple_members": [
    {"name": "@deepseek-ai/dsh", "version": "0.1.2-alpha.3"},
    {"name": "@deepseek-ai/dsh-acp-app", "version": "0.1.2-alpha.3"},
    {"name": "@agent-sessions/dsh-plugin", "version": "0.1.2-alpha.3"}
  ]
}
```

The package-manager name uses the shared bounded product-token grammar. The
tested product version, tuple-member versions, and package-manager version use
one bounded exact-version grammar; they are identities, never ranges. A manager
without its exact version, a version without its manager, or either outside an
exact non-empty tuple fails catalog validation.

Phase A adds `CapabilityParent` to the data catalog. A runtime `Parent` driver
is valid exactly when that capability is declared; it is not inferred. Product
capability values such as `parent` and opaque federation values such as
`opencode-lane` occupy distinct namespaces but pass through one bounded
lower-case token grammar and `productcatalog` validator.

## 2. Derived Consumers

The following consume that projection or the same Go descriptor API and do not
author product arrays:

- managed alias installation/removal;
- release package payload inventory;
- native integration install/remove plan;
- help and usage projection;
- doctor/roster product rows;
- federation capability advertisement;
- acceptance matrix expansion;
- user documentation version table;
- release evidence manifest.

CI compares all generated/derived views and fails on drift.

## 3. Install Strategy Registry

`internal/releaseinstall.Registry` maps descriptor install-strategy keys to
typed implementations. Like runtime composition, it is explicit and rejects
duplicates or missing strategies. It is release-time code and is not imported
by the running daemon.

Each strategy implements transactional phases:

```text
Discover -> CaptureBaseline -> Stage -> Validate -> Register -> Verify -> Commit
                                              \-> Rollback exact baseline
```

Absent native products produce a structured skip. A discovered incompatible
product produces a product-specific failure before host commit unless policy
explicitly permits installing assets for later activation.

## 4. Ownership Receipt

Every changed native registration/artifact records:

- product and strategy;
- canonical path or native registration key;
- prior exact identity/revision/type;
- installed exact identity/revision/type;
- rollback and removal preconditions;
- transaction/release ID.

No receipt contains credentials. User-modified current identity is preserved
and reported as debt rather than overwritten or removed.

## 5. Product-Specific Installation Rules

- OpenCode/Kilo: install exactly one Agent Sessions plugin location to avoid
  double loading; do not edit unrelated project config.
- Pi/OMP: install the Agent Sessions package/extension and skill through the
  supported user-global mechanism; project-local trust remains user-owned.
- CodeBuddy: install MCP assets and wrapper integration; peer discovery uses no
  password, while Agent Sessions-owned lane-server passwords remain transient.
- DSH: pnpm 10.28.1 is required; materialize one exact Agent Sessions-owned profile
  tuple and add the Cordis plugin only to explicitly named profiles with an
  ownership receipt. Never broadly rewrite all profiles.

## 6. Removal

Removal operates only on exact installed identities captured by receipts,
restores replaced prior registrations when still applicable, preserves data by
default, and reports changed artifacts as cleanup debt. Purge remains an
explicit separate operation.
