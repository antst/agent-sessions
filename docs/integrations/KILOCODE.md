# Kilo Code integration

Agent Sessions targets Kilo Code **7.5.6**.

The native plugin holds one `presence.sock` connection for each live Kilo
session and reports `{uuid,name,groups,product}`. It sends title changes,
receives live message delivery, and handles parent tools over that connection.
EOF removes the session. No component or broker protocol exists.

Each managed lane uses an isolated Kilo server and its documented HTTP/event
routes. Exact resume selects the known session through `kilo attach --session`
or the server's exact session-selection route. A missing session is an error;
the adapter never falls back to latest/continue.

Permission policy is applied through Kilo's native API. Doctor checks the exact
version, required routes, provider readiness, and the installed live-session
plugin. Native credentials remain process-local inputs to the product client.
