# Example workspace images

Containerfiles for projects that build their image (`--containerfile`).
Each one meets the requirements of plan section 3.8:

- `tmux`, `git` and `bash` are installed;
- user 1000 exists, with home `/home/agent`;
- the image runs as that user and keeps running (`sleep infinity`).

| Example | App port | Notes |
|---|---|---|
| `node/` | 3000 | Node.js 22; global npm packages go to the agent's home |
| `laravel/` | 8000 | PHP 8.3 with the extensions Laravel needs, Composer, Node.js for assets, the Redis extension |

Use the app port with `--app-port` to get a preview at
`https://<sandbox>--<project>.<preview domain>/`. Inside the sandbox the URL
is in `WSM_PREVIEW_URL`. The app must listen on `0.0.0.0` at that port.

The containers have no root and no capabilities. Install system packages
in the Containerfile, and rebuild with `wsm project image <slug>` (or
"Rebuild image" in the dashboard). Sandboxes created after the rebuild use
the new image.
