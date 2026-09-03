# Agent Sessions communications for DSH

`@agent-sessions/dsh-comms` publishes each live top-level DeepSeek Harness
session to the Agent Sessions presence socket. It adds `peers.list` and
`message.send` (one target, explicit targets, or a group), and delivers inbound messages through DSH's native
`Agent.steer` path.

A DSH product bundle includes the package by listing it as a bundle dependency
and inserting its plugin in the bundle patch. With no Agent Sessions launch
context or plugin `groups` configuration, the session is still presented under
its native session id with an empty name and no groups. This package does not
advertise or implement the native lane protocol.
