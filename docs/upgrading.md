# Upgrading ctxmesh

ctxmesh installs and upgrades as a Helm chart published as an OCI artifact. The chart's
`appVersion` is the product version; every first-party image carries the same tag, so an
install is reproducible and an upgrade is a single version change.

## Check what you are running

```sh
helm -n ctxmesh list
```

The `CHART` column is the chart version (`ctxmesh-0.1.0-beta.1`); `APP VERSION` is the
product version the images carry (`v0.1.0-beta.1`).

## Upgrade

```sh
helm upgrade ctxmesh oci://ghcr.io/ctxmesh/charts/ctxmesh \
  --version 0.1.0-beta.2 \
  --namespace ctxmesh \
  --wait --timeout 8m
```

`--wait` matters: it holds the command open until the rolled workloads are ready and the
post-upgrade preflight has run, so a failed upgrade reports as a failed command rather
than as a healthy-looking release with a crash-looping pod behind it.

Re-pass any `--set` flags your install uses. Helm carries stored values forward, but a
flag you passed at install time and omit here still applies — being explicit is what
makes the upgrade reproducible, and `helm get values ctxmesh -n ctxmesh` prints what the
current release is actually using.

## What is preserved

- **Your data.** The bundled Postgres keeps its PersistentVolumeClaim across upgrades;
  the chart never deletes it. ToolRegistries, MCP registrations, run history and the
  knowledge store are Postgres-backed and survive.
- **Your custom resources.** AgentDeployments, ModelRoutes, SecretBindings and the rest
  live in etcd and are untouched by a chart upgrade.
- **Your provider credentials.** The Secrets holding API keys are created by the connect
  flow, not by the chart, so an upgrade does not touch them.

The upgrade path is exercised on every release by `e2e-upgrade.sh`, which installs the
previous version from its published artifacts, writes real data through the product's own
API, runs **the command above**, and reads the data back. An upgrade on an empty cluster
proves nothing, so it is never tested that way.

## What to check afterwards

```sh
kubectl -n ctxmesh get pods
helm -n ctxmesh list
```

Every pod `Running`, and `APP VERSION` showing the version you upgraded to. If the
preflight Job failed, read it — it names the specific dependency it could not reach:

```sh
kubectl -n ctxmesh logs job/ctxmesh-preflight
```

## Rolling back

```sh
helm -n ctxmesh rollback ctxmesh
```

This returns the workloads to the previous release. It does **not** roll back data:
anything written since the upgrade stays written, and a rollback across a future release
that migrates the schema is not supported — check the release notes for that release
before upgrading, not after.

## Version skew

Upgrade one minor version at a time. Skipping versions is untested, and a schema
migration that assumes the immediately previous shape has no way to tell you it was
skipped.
