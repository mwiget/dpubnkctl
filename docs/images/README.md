# docs/images/

Screenshots + small diagrams referenced by the README, the slide deck,
and the upstream-issue write-ups.

Path convention: `docs/images/<topic>.png`. Referenced from markdown
as `images/<topic>.png` (the `docs/` prefix is stripped when GitHub
Pages renders).

Filenames in use:

| File | Used by | What it shows |
|---|---|---|
| `bnk-forge-2-3-0-view.png` | slide deck (Day-2 integration) | bnk-forge's view of the homelab-2-3-0 PoC after `dpubnkctl cluster up` registered the cluster: connected status, K8s v1.30, 4 nodes, FLO + CNE controller healthy |

Add new images by:
1. Drop the PNG in this folder
2. Reference it from the consuming markdown
3. Add a row to the table above
