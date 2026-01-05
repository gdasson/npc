# nodepatcher-final (reference implementation)

A Go controller-runtime operator that performs **node replacement (AMI patching) by cycling nodes** in a controlled,
idempotent way.

It implements:

- **NodePatchRun**: high-level orchestration
- **NodePatchTask**: per-node workflow (lease-locked, step-based, idempotent)
- **Provider interface** to keep most logic cloud/provider agnostic
- **AWS provider** implementation:
  - replacement instance in **same ASG** and **same AZ** as old instance
  - instance tags for idempotency + cleanup (run UID, task name, state)
  - optional instance protection while joining
- **Two-iteration mixed-AZ plan**: run partitions are mixed across AZs and processed in 2 iterations (50% then 50%)
- **Important pods gating** (label selectors from CR spec) + stability window
- **Required daemon pod readiness on new node** (e.g., CNI, kube-proxy)
- **Delta label transfer** old node -> new node (only selected keys/prefixes; does not overwrite existing labels)
- **Cluster Autoscaler control**: scales CA Deployments to 0 at start, restores at end or finalizer

> ⚠️ This code can terminate instances / nodes. Treat as a reference skeleton for production hardening.

## Build & run

This repo includes `go.mod` but does **not** include `go.sum` in this sandbox environment.
When you build locally, run:

```bash
go mod tidy
go test ./...
```

Run locally against a cluster:

```bash
go run ./main.go
```

Environment variables:
- `LOCK_NAMESPACE`: namespace to store node leases (default `nodepatcher-system`)
- `POD_NAME`: appended to lease holder identity (optional)

## CRDs

See Go types:
- `api/v1alpha1/nodepatchrun_types.go`
- `api/v1alpha1/nodepatchtask_types.go`

Example CR:
- `examples/nodepatchrun-aws.yaml`

## How it works (high level)

### NodePatchRun
1. Optionally **scales down cluster-autoscaler** and records original replicas in status + annotations.
2. Selects candidate nodes (optional `nodeSelector`, `excludeNodeSelector`, `excludeControlPlane`).
3. Builds a **mixed-AZ 2-partition plan** and stores it in `status.plan`.
4. For the current iteration, computes **important nodes** dynamically from your pod selectors and creates **NodePatchTask**
   objects up to:
   - `batchSizeImportant`
   - `batchSizeNonImportant`
   - optional `maxConcurrentPerZone`
5. When all nodes in the iteration succeeded, advances to the next iteration.
6. After all iterations, restores cluster-autoscaler and marks Completed.

### NodePatchTask (per node)
A step machine guarded by a **Lease** keyed on the *old node name*:

1. Resolve old node -> provider machine reference.
2. Cordon old node.
3. Ensure replacement machine exists (provider must be idempotent).
4. Wait for new node to appear + Ready + required daemons ready.
5. Transfer delta labels.
6. Optional stability gate (important pods healthy for N duration).
7. Drain old node.
8. Delete old machine, unprotect new, mark Succeeded.

## Notes / production hardening ideas

- Add event recording for better observability.
- Use Conditions for Tasks (not just phase string).
- Add timeouts per phase and a retry budget.
- Support multiple ASGs explicitly via selectors/annotations.
- Add provider implementations (e.g., VMware) by implementing `pkg/provider.Provider`.

