-- 059_config_global_unique.up.sql — Unique index for global config variables.
--
-- config_variables carries UNIQUE(name, scope, scope_id), but global settings
-- store scope_id = NULL and Postgres treats NULLs as distinct there, so the
-- constraint never fires for them. The repository's Upsert relies on
-- "ON CONFLICT (name, scope) WHERE scope_id IS NULL", which requires a
-- matching partial unique index; without it every settings save failed with
-- "there is no unique or exclusion constraint matching the ON CONFLICT
-- specification".
--
-- Duplicates may already exist from earlier plain inserts, so keep the most
-- recently updated row per (name, scope) before creating the index.

DELETE FROM config_variables cv
USING config_variables newer
WHERE cv.scope_id IS NULL
  AND newer.scope_id IS NULL
  AND newer.name = cv.name
  AND newer.scope = cv.scope
  AND (newer.updated_at > cv.updated_at
       OR (newer.updated_at = cv.updated_at AND newer.id > cv.id));

CREATE UNIQUE INDEX IF NOT EXISTS idx_config_variables_global_unique
    ON config_variables (name, scope)
    WHERE scope_id IS NULL;
