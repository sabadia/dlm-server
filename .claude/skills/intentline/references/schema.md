# Record fields (schema v1)

A record is about twenty lines of YAML at `.intentline/records/<id>.yml`. The full contract is `docs/03-schema-cli-hooks.md` in the intentline repository. Do not add keys.

| Field | What to put |
|---|---|
| `status` | `draft` until a human affirms. Agents only draft. |
| `goal` | The user's stated goal. Required. One line. |
| `constraints` | Limits the user stated. Empty list if they named none. |
| `rejected` | Options the user said they considered and rejected. Empty unless they typed them. |
| `assumptions` | Assumptions the user stated. Empty unless they typed them. |
| `falsifier` | What the user said would prove this wrong. Empty string unless they typed it. |
| `links` | Repo-relative paths (and optional lines) the change touches. At least one. |
| `risks` | Risks the user named. Empty unless they named them. |
| `author.kind` | `agent` when an agent captures, with `author.tool` set (for example `claude-code`). |

`intentline capture` leaves out anything you do not pass. Do not fill the gaps.
