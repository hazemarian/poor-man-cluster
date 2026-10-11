# Test reports (TC1–TC10)

Field reports from live campaigns on real hardware, in chronological order.
One file per campaign. These are **historical records** — they document what
was tested, what broke, and what shipped as a result, at the release current
when the campaign ran.

| # | Campaign | Nodes | Focus |
|---|---|---|---|
| TC1 | [Single-manager bring-up](TC1-single-manager-bringup.md) | 1 | First bring-up, multi-service deploy, observability, UI, control plane |
| TC2 | [Backup/restore, S3, ACME](TC2-backup-restore-s3-acme.md) | 1 | Custom volume root, Let's Encrypt (HTTP-01 through an LB), backup/restore local + S3 |
| TC3 | [Webhook + CLI features](TC3-webhook-cli-features.md) | 1 | Webhook deploys, CLI surface |
| TC4 | [Single-manager misc](TC4-single-manager-misc.md) | 1 | Remaining single-manager feature checks |
| TC5 | [Single-manager regressions](TC5-single-manager-regressions.md) | 1 | Regression pass |
| TC6 | [Single-manager remaining](TC6-single-manager-remaining.md) | 1 | Final single-manager coverage |
| TC7 | [Two-node manager+worker](TC7-two-node-manager-worker.md) | 2 | First worker join, multi-node placement |
| TC8 | [Two-node storage HA](TC8-two-node-storage-ha.md) | 2 | Storage-node model, backups across nodes |
| TC9 | [Two-node backup store](TC9-two-node-backup-store.md) | 2 | The in-cluster backup store |
| TC10 | [Four-node storage HA + preprod](TC10-four-node-storage-ha-and-preprod.md) | 4 | Storage HA, leader failover, the pre-production campaign |

How these campaigns feed the bug catalogue and the testing doctrine
(verify → tag → rollout) is covered in the
[RFC](../rfc/README.md) — [ch. 04, bug catalogue](../rfc/04-bug-catalogue.md)
and [ch. 06, testing methodology](../rfc/06-testing-methodology.md).
