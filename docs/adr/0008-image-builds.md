# 0008: Project images: builds and pulls

## Status

Accepted.

## Context

M2 (plan section 5) lets a project get its workspace image in two ways: from a Containerfile that the owner provides, or from a registry. Requirements "Projecten, images en sandboxes" and assumptions 8 and 9 apply. The image is produced by a process that the project user controls, and its output is untrusted (CLAUDE.md invariant 8). The Containerfile is part of the ceiling (invariant 5), so it must not live anywhere an agent can write.

The preflight of M2a showed on the VM that a build through the libpod REST API, sent by `wsm` to the project user's Podman socket, works. The image lands in the project user's storage, and RUN steps go through the wsm egress rules: a connection to the LAN moved the `lan_blocked` counter, while the internet stayed open.

## Decision

1. **Build through the API, as the project user.** wsm sends `POST /libpod/build?t=<tag>&dockerfile=Containerfile&rm=true` to the project user's Podman socket (ADR 0002). The build runs as that user, with Podman's defaults for networking, capabilities and isolation. Its traffic is therefore that user's traffic, and the per-UID egress rules of ADR 0001 apply to RUN steps. `podman.ImageBuild` has no parameter for host networking, privileged mode, extra capabilities or volumes, just as `ContainerSpec` has none.
2. **The context holds only the Containerfile.** wsm creates the tar in memory with a single entry, `Containerfile`, and streams it. `COPY` and `ADD` of local files therefore have nothing to copy. `ADD` of a URL still works and is subject to the egress rules. The project's code comes into the sandbox later, as a bundle (M2b), not through the image.
3. **The Containerfile lives only in the database.** It is stored in `projects.containerfile` (migration 00003) and is changed only through the API. It is at most 256 KiB, valid UTF-8 and free of NUL bytes; both the store and a CHECK constraint enforce the size. wsm never writes it to disk. Podman unpacks the build context into its own temporary directory for the duration of the build. That copy belongs to the project user's Podman and is never read back by wsm: the database copy is what the next build uses.
4. **The tag comes from the content.** A Containerfile project's image is `localhost/wsm-<project id>:<first 12 hex characters of sha256(Containerfile)>`. The same Containerfile maps to the same tag. A changed Containerfile gets a new tag, so an image built from an older version is never mistaken for the current one.
5. **`sandbox create` makes sure the image is there.** If the image is missing, wsm pulls a registry image (policy `missing`) or builds the tag of a Containerfile project. If that fails, the sandbox is rolled back, as any other create failure in M1. `project create` neither pulls nor builds. `wsm project image SLUG` builds or pulls again right away (policy `always` for a pull), as a job, and reports the image and its ID. One pull or build is bounded by a 30-minute timeout.
6. **Only public registries.** wsm sends no registry credentials and stores none. Private registries are out of scope for phase 1.
7. **The output is untrusted.** The pull and build progress streams are capped at 16 MiB and decoded message by message into a typed struct. An error message in the stream fails the call. The error quotes the message and at most 4 KiB of the preceding output, with control characters replaced, so the output cannot drive the owner's terminal. The image ID comes from inspecting the tag afterwards (and must be 64 hex characters), not from the stream.
8. **Re-creating sandboxes on a change comes later** (assumption 9). New sandboxes get the current image. Existing sandboxes keep the container they were created with.

## Consequences

- The project user's storage holds base images and build layers, counted against that user's disk. Images built from an older Containerfile stay as dangling images until the project is deleted. Cleaning them up is follow-up work.
- The manager runs one state-changing operation at a time (M1). A long build therefore holds up other operations, at most for the 30-minute timeout.
- Unknown fields in stream messages are ignored rather than rejected, so a newer Podman that adds fields does not break builds. Every field wsm reads is typed, and the stream as a whole is bounded.
- Isolation tests `TestBuildRunsAsProjectUser`, `TestBuildNetworkFiltered`, `TestSandboxOnBuiltImage`, `TestRegistryImagePulledByProjectUser` and `TestManagerStateNotReachable` cover this decision on the VM.
