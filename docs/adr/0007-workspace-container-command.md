# 0007: Command of the workspace container

## Status

Accepted.

## Context

Plan section 3.4 says the workspace container runs `sleep infinity` when the image has no CMD, and the image's CMD otherwise. It does not mention ENTRYPOINT. Many images start through an ENTRYPOINT alone, for example a script that starts several services. If wsm set a command for such an image, Podman would pass `sleep infinity` to that entrypoint as arguments, and the image would not start the way it was built to.

## Decision

The workspace container keeps the image's own start: its CMD, its ENTRYPOINT, or both. Only an image with neither gets `sleep infinity`, so that the container keeps running for exec and terminal sessions. The container still starts with `--init`.

## Consequences

- An image with only an ENTRYPOINT runs that entrypoint. When it exits, the sandbox stops, just as with a CMD that exits.
- `workCommand` in `internal/sandbox` implements the rule; `TestImageCommandIsKept` covers CMD, ENTRYPOINT and both.
- This refines plan section 3.4: "geen CMD" there means neither CMD nor ENTRYPOINT.
