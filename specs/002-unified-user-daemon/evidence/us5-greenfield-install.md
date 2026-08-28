# US5: Greenfield First-Install Boundary

Version 0.3 deliberately starts from a clean Agent Sessions-owned host boundary. Before first install,
the operator stops every unreleased prototype peer, lane, supervisor, manager, shim, and host agent,
then removes or archives their Agent Sessions-owned state, runtime, release, connector, and service
artifacts.

The product does not inventory, adopt, translate, drain, retire, or recover the prototype topology.
It exposes no migration command and contains no fallback reads from prototype stores. Vendor-owned
credentials, settings, transcripts, histories, and native session stores are outside this cleanup and
remain untouched.

The acceptance contract is therefore intentionally small and strict:

- installation starts only after the operator establishes the clean boundary;
- the fresh service creates one host authority and one local control endpoint;
- connector installation targets the fresh 0.3 release;
- restart and upgrade recovery use only the unified state schema;
- normal removal preserves unified configuration and state, while explicit purge removes only the
  revision-bound unified role roots; and
- source, CLI, help, package, and process inventories contain no compatibility implementation or
  obsolete authoritative executable.

Earlier fixture evidence for prototype-state adoption and retirement is withdrawn and receives no
acceptance credit. It tested a requirement that the final specification explicitly rejects.
