# Upgrade Proposal: v0.2.7

This document outlines the proposed changes for on-chain software upgrade v0.2.7. The `Changes` section details the major modifications, and the `Upgrade Plan` section describes the process for applying these changes.

## Upgrade Plan

This PR updates the code for the `api` and `node` services. The PR modifies the container versions in `deploy/join/docker-compose.yml`.

The binary versions will be updated via an on-chain upgrade proposal. For more information on the upgrade process, refer to [`/docs/upgrades.md`](https://github.com/gonka-ai/gonka/blob/upgrade-v0.2.7/docs/upgrades.md).

Existing hosts are **not** required to upgrade their `api` and `node` containers. The updated container versions are intended for new hosts who join after the on-chain upgrade is complete.

## Proposed Process

1. Active hosts review this proposal on GitHub.
2. Once the PR is approved by a majority, a `v0.2.7` release will be created from this branch, and an on-chain upgrade proposal for this version will be submitted.
3. If the on-chain proposal is approved, this PR will be merged immediately after the upgrade is executed on-chain.

Creating the release from this branch (instead of `main`) minimizes the time that the `/deploy/join/` directory on the `main` branch contains container versions that do not match the on-chain binary versions, ensuring a smoother onboarding experience for new hosts.


Start after upgrade:
```
git pull
source config.env && docker compose -f docker-compose.postgres.yml up -d
```

## Testing

<>

## Migration

<>

## Changes

---

### Changes Name 1

Commit: [<hash>](https://github.com/gonka-ai/gonka/commit/<hash>)

<DESCIPTIOPN>

---

### Changes Name 2

Commit: [<hash>](https://github.com/gonka-ai/gonka/commit/<hash>)

<DESCIPTIOPN>
