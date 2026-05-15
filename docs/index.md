---
title: dpubnkctl
---

# dpubnkctl

F5 BIG-IP Next for Kubernetes — deploy in one binary, drive with an agent.

Targets **BNK 2.3.0** on NVIDIA BlueField-3 DPUs (DOCA 3.2.0, Ubuntu 24.04). Maintenance fixes for 2.2.0 live on the `release-2.2.0` branch.

## Slide decks

- [Overview deck (web)](slides/dpubnkctl-overview.html) — what dpubnkctl is, why it exists, the human + agentic operating modes, the homelab case study (v2.2.0 round + v2.3.0 migration), and the deploy-confirmed proof. ~23 slides; arrow keys to navigate, `f` for fullscreen, `o` for overview.
- [Overview deck (markdown source)](slides/dpubnkctl-overview.md) — readable as a long doc on GitHub; also the source `make slides{-html,-pptx,-pdf}` builds from.

## Source

- Repository: [github.com/mwiget/dpubnkctl](https://github.com/mwiget/dpubnkctl)
- Releases: [github.com/mwiget/dpubnkctl/releases](https://github.com/mwiget/dpubnkctl/releases)

## One-liner install (Linux amd64, from source)

```
go install github.com/mwiget/dpubnkctl/cmd/dpubnkctl@latest
```

## A reproducible PoC in two commands

```
dpubnkctl init mycustomer
dpubnkctl e2e --yolo
```

(`init` creates a per-PoC repo, `e2e` runs the full destructive
pipeline against `poc.yaml`. ~75 min, resume-safe. See the
[overview deck](slides/dpubnkctl-overview.html) for the full picture.)
