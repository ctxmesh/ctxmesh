# Project-owned OTel Collector image.
#
# Why we repackage instead of using otel/opentelemetry-collector directly:
# the upstream distroless images ship a dynamically-linked binary but (at least
# on the arm64 variant of 0.116.0) omit the glibc loader
# /lib/ld-linux-<arch>.so.1, so the binary fails with
# "exec /otelcol: no such file or directory". debian:12-slim provides the
# loader, so the same binary runs on both arm64 (local) and amd64 (CI).
#
# We use the CORE collector (not contrib): M3 only needs otlp/batch/debug/
# otlphttp, all in core, and core is ~10x smaller than contrib.
FROM otel/opentelemetry-collector:0.116.0 AS upstream

FROM debian:12-slim
COPY --from=upstream /otelcol /otelcol
# distroless upstream ships default config at /etc/otelcol/config.yaml; the
# controller mounts ours at /etc/otel/config.yaml and passes --config.
ENTRYPOINT ["/otelcol"]
