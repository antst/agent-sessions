# S5 legacy reachability audit

Run from the repository root:

```sh
scripts/spikes/six-product/legacy/run.sh > /tmp/S5-raw.json
```

The scanner inventories direct production/test importers and external selector
references for `internal/bridge` and `internal/federator`, records the three
legacy runner entrypoints, parses a pinned `deadcode` result for the two live
commands, compares the two independently authored product tables, and detects
the stale Forgejo command paths.

The three legacy runner declarations are co-located with shared declarations.
The bounded removal probe demonstrates that none of those source files is an
independent deletion unit:

```sh
for file in internal/bridge/runtime.go internal/federator/agent.go internal/federator/hub.go; do
  scripts/spikes/six-product/legacy/probe-file-removal.sh "$file"
done
```

`deadcode` is evidence about unreachable functions, not permission to delete a
whole source file. The S5 decision therefore treats file deletion as certified
only when call inventory, package compilation, same-package tests, and the
normal/race/vet/lint gates agree. The phase-zero evidence records why no
production-file deletion is certified on the frozen base and names the focused
extraction boundary instead.
