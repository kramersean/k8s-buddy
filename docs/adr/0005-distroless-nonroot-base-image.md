# 5. Distroless nonroot base image for buddy-api

## Status

Accepted

## Context

`build/Dockerfile.buddy-api` is a multi-stage build: a `golang:1.26-alpine`
builder stage that cross-compiles a static, CGO-free binary, and a final stage
that ships it. The question is what the final stage should be built `FROM`.

The realistic options:

- **`alpine`** — small (~8MB), but ships busybox, apk, and a shell. Every one of
  those is attack surface the application never uses, and each is a source of
  CVEs that the security scan in CI will flag on a dependency the binary does
  not link against.
- **`scratch`** — the absolute minimum, but it has no CA certificate bundle, no
  tzdata, and no `/etc/passwd` entry. A container with no passwd entry can be
  told to run as UID 65532, but tooling that resolves the user by name cannot,
  and the missing CA bundle would break any future outbound TLS silently at
  the first attempt.
- **`gcr.io/distroless/static-debian12:nonroot`** — CA certificates, tzdata, and
  a passwd entry for uid/gid 65532, and nothing else. No shell, no package
  manager, no libc, no OS userland.

The binary is built with `CGO_ENABLED=0`, so it has no dynamic-linking
requirement that would rule out a static base.

## Decision

The final stage is `FROM gcr.io/distroless/static-debian12:nonroot`, with
`USER 65532:65532` set explicitly in the Dockerfile rather than relying on the
tag's default.

## Consequences

- There is no shell in the image, so `docker run --entrypoint sh` fails and
  `kubectl exec -- /bin/sh` fails. An attacker who achieves code execution
  inside the container finds no interactive tooling to pivot with. Verified by
  running `docker run --rm --entrypoint sh` against the built image and
  observing the exec failure.
- The image satisfies Pod Security Admission's `restricted` profile as shipped:
  it runs as a non-root UID with `readOnlyRootFilesystem: true`,
  `allowPrivilegeEscalation: false`, and all capabilities dropped, with no
  chown or writable-path workarounds needed.
- The trivy image scan in CI has essentially no OS-package surface to report
  on, so a HIGH/CRITICAL finding there points at the Go dependency tree — the
  thing this project actually controls — rather than at Debian packages nobody
  is going to patch in a demo repository.
- The direct cost: no exec-based `lifecycle.preStop` hook is possible, which is
  what forced the in-process shutdown delay recorded in ADR 0002. Any future
  component that assumes a shell in the runtime image — an init wrapper, an
  entrypoint script, a `command: ["sh", "-c", ...]` — will not work and must be
  reworked into the binary or given a different base image.
- Debugging a running pod requires `kubectl debug` with an ephemeral container,
  not `kubectl exec`. That is a real ergonomic cost, accepted deliberately.
