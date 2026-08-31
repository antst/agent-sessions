# Specification Quality Checklist: Unified User Daemon

**Purpose**: Validate specification completeness before planning
**Created**: 2026-08-29
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] Focused on preserving user-visible value while changing topology
- [x] All mandatory sections completed
- [x] Native-product implementation ownership is described only where required to define compatibility
- [x] The working baseline is identified as executable authority

## Requirement Completeness

- [x] No clarification markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions are identified

## Behavioral-Parity Readiness

- [x] Every replacement requires old-symbol and old-test traceability
- [x] DRY generalization is subordinate to observed parity
- [x] Product-specific native behavior is explicitly retained
- [x] Real installed Linux and macOS evidence precedes legacy deletion
- [x] Fake-vendor tests cannot receive product-parity credit
- [x] The complete 202-cell baseline is closed, machine-readable, and individually reportable
- [x] Every acceptance cell expands to platform scope, one exact assertion locator, and explicit acyclic prerequisites
- [x] Every changed Agent Sessions topology observation is confined to a reviewed old/new/preserved-invariant ledger entry
- [x] Verdict-conditional evidence, prerequisite propagation, and authoritative rerun supersession are defined
- [x] Port-map statuses use documented cumulative predicates rather than unprovable scalar history
- [x] Version 0.3 contains no legacy-stack discovery or compatibility subsystem; quiescence is an operator/harness precondition
- [x] The three-host cleanup utility has one repository-only exact allowlist contract, defaults to mutation-free planning, requires explicit apply with immediate metadata-revision revalidation, may purge legacy-owned opaque data without reading it, and is excluded from operational Make targets, standard lifecycle paths, and release packages
- [x] No speculative capacity, recovery-time, observability, purge, or host-release rollback surface is introduced

## Notes

The specification intentionally includes native-product compatibility constraints because this feature
refactors an existing integration product. Omitting those constraints previously allowed a generic
design to replace observed vendor behavior.

The acceptance inventory is exactly 202 unique stable IDs, with assertions retained in
`docs/ACCEPTANCE-MATRIX.md`. `contracts/acceptance-matrix.yml` machine-validates cardinality, platform
scope, assertion location, prerequisite closure, topology substitutions, and result-credit rules for
every expanded cell.
