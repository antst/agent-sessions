# Raw protocol-v1 peers

```sh
go install ./cmd/verbatim-writer ./cmd/ingress
verbatim-writer --name scribe --group team
ingress --group team
ingress --group team --target scribe
```

`verbatim-writer` emits one JSON object per delivery with `time`, `message_id`, `from`, and `body`; time is UTC RFC3339Nano. `ingress` sends each non-empty stdin line, strips a trailing CR, logs and skips invalid UTF-8, and stops on an oversized line or first send error. Because ingress intentionally has no readiness gate, keep stdin open until `agent-sessions roster` shows its `ingress` session.
