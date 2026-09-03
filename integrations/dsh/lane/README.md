# Agent Sessions native lanes for DSH

`@agent-sessions/dsh-lane` adds Agent Sessions native lane operations to a DSH
profile that also loads `@agent-sessions/dsh-comms`. Its hard Cordis service
dependency keeps the lane adapter pending until the communications plugin is
live.

This package is intended for the daemon-owned `agent-sessions` DSH profile.
Other DSH bundles should include only `@agent-sessions/dsh-comms` when they need
peer presence and messaging without native lane control.
