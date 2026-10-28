# Schema migrations

One file per version, `NNNN_description.sql`, applied in order and embedded in the binary. The number must continue the sequence without gaps (a test checks it). Never edit a migration that has shipped: add a new one.

## Rule: stay compatible with the previous release (expand/contract)

An image rollback after a migration must keep working, so every migration has to be safe for the code of the release before it:

- **Expand first**: add tables, add columns that are nullable or have a default, add indexes. The previous release ignores them.
- **Contract later**: drop or rename a column, add a NOT NULL without a default, or change a meaning only in a later release, after no supported version reads the old shape.
- A change that cannot be done in two steps needs a note in `docs/operations.md` saying that rollback past that version means restoring a backup.

Statements are separated by `;`. Comments, quoted strings and `$$` blocks are respected by the splitter, but keep each statement idempotent (`IF NOT EXISTS`) so a migration can be re-run safely.
