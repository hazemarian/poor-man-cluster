<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Deploy Workflow

### 1. Create the manifest

Write a `.yaml` manifest file for your service. Start from the template above.

### 2. Validate

`pmcluster deploy` validates and translates the DSL in one step; malformed manifests fail before any Swarm change. To inspect the generated Compose without deploying, deploy first and read the rendered manifest from the revision (`pmcluster stack show <app>` or the console's revision view).

### 3. Deploy

```bash
pmcluster deploy ./manifest.yaml

# Override version at deploy time:
pmcluster deploy ./manifest.yaml --version v1.2.3

# Override app name:
pmcluster deploy ./manifest.yaml --app custom-name

# Attach a repo URL (metadata):
pmcluster deploy ./manifest.yaml --repo https://github.com/org/repo
```

Each deploy creates a new revision keyed by Unix timestamp.

### 4. Verify

```bash
pmcluster stack list                        # all deployed stacks
pmcluster stack show my-app                 # metadata + revision history (→ marks current)
docker service ls                           # raw Swarm view
docker service ps my-app_api                # container-level status
```


## Rollback

```bash
pmcluster stack show my-app                 # find the revision you want
pmcluster rollback my-app 1719000000        # redeploy that revision
```

Rollback creates a NEW revision (preserves audit trail) pointing at the old compose. Both forward and backward deploys are recorded.
