# Contract: Federation Compatibility

The existing topology remains one central hub with multiple embedded host agents. Each host agent runs
inside that user's `agent-sessions` daemon. The central deployment runs `agent-sessions-hub`.

- Global groups are the sole collaboration visibility boundary.
- Host suffixes disambiguate peer addresses and create no namespace or access rule.
- AgentFrame identity, provenance, direct send, multicast, broadcast, reply, remote lane dispatch,
  result notification, and idempotency retain baseline behavior.
- Host and hub software versions are compatible only when their exact hub protocol versions match.
  Commit, release, build, and installation age are diagnostic only.
- A mismatch is rejected before host registration, delivery, or lane acceptance.
- Restart or reconnect retains exact host identity and does not duplicate accepted work.
- The hub owns no vendor profile, attachment, lane-native, credential, transcript, or host-service state.
