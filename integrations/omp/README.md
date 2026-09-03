# Agent Sessions for Oh My Pi

This package exposes the Agent Sessions extension entrypoint for Oh My Pi.

The package intentionally bundles its own packaged copy of the shared Pi-family
adapter. It does not depend on `@agent-sessions/pi`: keeping the OMP
artifact self-contained lets either product integration be installed, tested,
and upgraded independently.
