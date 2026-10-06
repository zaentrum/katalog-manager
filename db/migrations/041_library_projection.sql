-- 041_library_projection.sql — a change of what a projection shows marks it (idempotent).
--
-- The library's projections, an item's metadata.json and a person's
-- person.json, are written by katalog-manager's projector from the database,
-- and only when what they show changed: an item's modifiedat newer than its
-- libraryprojectedat (migration 040), a person's likewise. These triggers keep
-- modifiedat telling that, whoever writes the rows:
--   - a statement that inserts, changes or deletes an item's genres, tags,
--     credits, reference ids, images or trailer links marks each item it
--     touched once, however many of its rows it touched (statement-level
--     triggers with transition tables, one per event);
--   - one that changes how an extra is shown (its order, whether it is
--     hidden, its label), removes it or records it marks its title; Postgres
--     takes no column list with transition tables, so the trigger compares
--     those columns itself;
--   - a change of a projected column of an item (its type, title, sort
--     title, year, overview, rating, runtime, parent, numbers, tagline or
--     lock) marks it, unless the statement sets modifiedat itself; a change of
--     recordedat or libraryprojectedat alone does not;
--   - an episode inserted or deleted, or moved to another season or parent,
--     marks its series, whose seasons it lists;
--   - a change of a person's images marks the person, and a change of their
--     name the titles that credit them.
-- The tables of migrations 030 and 039 get theirs only where they exist.
-- Lowercase identifiers, like every com_nalet_katalog_* table.

-- The items whose rows a statement on one of their tables touched.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_items_touched() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE id IN (SELECT item_id FROM library_new);
  ELSIF TG_OP = 'DELETE' THEN
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE id IN (SELECT item_id FROM library_old);
  ELSE
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE id IN (SELECT item_id FROM library_old UNION SELECT item_id FROM library_new);
  END IF;
  RETURN NULL;
END $$;

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['com_nalet_katalog_itemgenres', 'com_nalet_katalog_itemtags',
      'com_nalet_katalog_itempeople', 'com_nalet_katalog_itemexternalids',
      'com_nalet_katalog_itemartworkdata', 'com_nalet_katalog_itemtrailerlinks'] LOOP
    EXECUTE format('DROP TRIGGER IF EXISTS library_insert ON %I', t);
    EXECUTE format('CREATE TRIGGER library_insert AFTER INSERT ON %I REFERENCING NEW TABLE AS library_new '
      'FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
    EXECUTE format('DROP TRIGGER IF EXISTS library_update ON %I', t);
    EXECUTE format('CREATE TRIGGER library_update AFTER UPDATE ON %I REFERENCING OLD TABLE AS library_old '
      'NEW TABLE AS library_new FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
    EXECUTE format('DROP TRIGGER IF EXISTS library_delete ON %I', t);
    EXECUTE format('CREATE TRIGGER library_delete AFTER DELETE ON %I REFERENCING OLD TABLE AS library_old '
      'FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
  END LOOP;
END $$;

-- The titles whose extras a statement showed otherwise, removed or recorded
-- (an extra given to another title marks both).
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_extras_touched() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE com_nalet_katalog_items SET modifiedat = now()
  WHERE id IN (SELECT unnest(ARRAY[o.item_id, n.item_id]) FROM library_old o JOIN library_new n ON n.id = o.id
               WHERE o.sortorder IS DISTINCT FROM n.sortorder OR o.hidden IS DISTINCT FROM n.hidden
                  OR o.label IS DISTINCT FROM n.label OR o.removedat IS DISTINCT FROM n.removedat
                  OR o.recordedat IS DISTINCT FROM n.recordedat OR o.item_id IS DISTINCT FROM n.item_id);
  RETURN NULL;
END $$;

DO $$
BEGIN
  IF to_regclass('com_nalet_katalog_itemextras') IS NOT NULL THEN
    DROP TRIGGER IF EXISTS library_update ON com_nalet_katalog_itemextras;
    CREATE TRIGGER library_update AFTER UPDATE ON com_nalet_katalog_itemextras
      REFERENCING OLD TABLE AS library_old NEW TABLE AS library_new
      FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_extras_touched();
  END IF;
END $$;

-- An item whose projected columns changed, unless the statement says when.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_item_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.modifiedat IS NOT DISTINCT FROM OLD.modifiedat AND (
       NEW.type IS DISTINCT FROM OLD.type OR NEW.title IS DISTINCT FROM OLD.title
    OR NEW.sorttitle IS DISTINCT FROM OLD.sorttitle OR NEW.year IS DISTINCT FROM OLD.year
    OR NEW.description IS DISTINCT FROM OLD.description OR NEW.rating IS DISTINCT FROM OLD.rating
    OR NEW.durationms IS DISTINCT FROM OLD.durationms OR NEW.parent_id IS DISTINCT FROM OLD.parent_id
    OR NEW.seasonnumber IS DISTINCT FROM OLD.seasonnumber OR NEW.episodenumber IS DISTINCT FROM OLD.episodenumber
    OR NEW.tagline IS DISTINCT FROM OLD.tagline OR NEW.metadatalocked IS DISTINCT FROM OLD.metadatalocked) THEN
    NEW.modifiedat := now();
  END IF;
  RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS library_changed ON com_nalet_katalog_items;
CREATE TRIGGER library_changed BEFORE UPDATE ON com_nalet_katalog_items
  FOR EACH ROW EXECUTE FUNCTION com_nalet_katalog_library_item_changed();

-- The series of the episodes a statement inserted or deleted: the parent when
-- it is the series, else the parent's parent (a season's series).
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_episodes_touched() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE type = 'series' AND id IN (
      SELECT CASE WHEN p.type = 'series' THEN p.id ELSE p.parent_id END
      FROM library_new e JOIN com_nalet_katalog_items p ON p.id = e.parent_id WHERE e.type = 'episode');
  ELSE
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE type = 'series' AND id IN (
      SELECT CASE WHEN p.type = 'series' THEN p.id ELSE p.parent_id END
      FROM library_old e JOIN com_nalet_katalog_items p ON p.id = e.parent_id WHERE e.type = 'episode');
  END IF;
  RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS library_episodes_insert ON com_nalet_katalog_items;
CREATE TRIGGER library_episodes_insert AFTER INSERT ON com_nalet_katalog_items
  REFERENCING NEW TABLE AS library_new
  FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_episodes_touched();
DROP TRIGGER IF EXISTS library_episodes_delete ON com_nalet_katalog_items;
CREATE TRIGGER library_episodes_delete AFTER DELETE ON com_nalet_katalog_items
  REFERENCING OLD TABLE AS library_old
  FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_episodes_touched();

-- An episode moved to another season or parent marks the series it left and
-- the one it is in. Rare, so a row trigger, which its condition keeps idle
-- for every other update of an item.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_episode_moved() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE com_nalet_katalog_items SET modifiedat = now()
  WHERE type = 'series' AND id IN (
    SELECT CASE WHEN p.type = 'series' THEN p.id ELSE p.parent_id END
    FROM com_nalet_katalog_items p WHERE p.id IN (OLD.parent_id, NEW.parent_id));
  RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS library_episode_moved ON com_nalet_katalog_items;
CREATE TRIGGER library_episode_moved AFTER UPDATE ON com_nalet_katalog_items
  FOR EACH ROW WHEN ((OLD.type = 'episode' OR NEW.type = 'episode') AND (OLD.seasonnumber IS DISTINCT FROM NEW.seasonnumber
    OR OLD.parent_id IS DISTINCT FROM NEW.parent_id OR OLD.type IS DISTINCT FROM NEW.type))
  EXECUTE FUNCTION com_nalet_katalog_library_episode_moved();

-- The people whose images a statement touched.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_portraits_touched() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE com_nalet_katalog_people SET modifiedat = now()
    WHERE id IN (SELECT person_id FROM library_new);
  ELSIF TG_OP = 'DELETE' THEN
    UPDATE com_nalet_katalog_people SET modifiedat = now()
    WHERE id IN (SELECT person_id FROM library_old);
  ELSE
    UPDATE com_nalet_katalog_people SET modifiedat = now()
    WHERE id IN (SELECT person_id FROM library_old UNION SELECT person_id FROM library_new);
  END IF;
  RETURN NULL;
END $$;

-- The titles that credit a person whose name changed: their credits show it.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_person_renamed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE com_nalet_katalog_items SET modifiedat = now()
  WHERE id IN (SELECT item_id FROM com_nalet_katalog_itempeople WHERE person_id = NEW.id);
  RETURN NULL;
END $$;

DO $$
BEGIN
  IF to_regclass('com_nalet_katalog_personartwork') IS NOT NULL THEN
    DROP TRIGGER IF EXISTS library_insert ON com_nalet_katalog_personartwork;
    CREATE TRIGGER library_insert AFTER INSERT ON com_nalet_katalog_personartwork
      REFERENCING NEW TABLE AS library_new
      FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_portraits_touched();
    DROP TRIGGER IF EXISTS library_update ON com_nalet_katalog_personartwork;
    CREATE TRIGGER library_update AFTER UPDATE ON com_nalet_katalog_personartwork
      REFERENCING OLD TABLE AS library_old NEW TABLE AS library_new
      FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_portraits_touched();
    DROP TRIGGER IF EXISTS library_delete ON com_nalet_katalog_personartwork;
    CREATE TRIGGER library_delete AFTER DELETE ON com_nalet_katalog_personartwork
      REFERENCING OLD TABLE AS library_old
      FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_portraits_touched();
  END IF;
END $$;

DROP TRIGGER IF EXISTS library_renamed ON com_nalet_katalog_people;
CREATE TRIGGER library_renamed AFTER UPDATE ON com_nalet_katalog_people
  FOR EACH ROW WHEN (OLD.name IS DISTINCT FROM NEW.name)
  EXECUTE FUNCTION com_nalet_katalog_library_person_renamed();
