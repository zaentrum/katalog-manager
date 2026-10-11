-- 046: the title the scanner gave a series when it made it.
--
-- The scanner finds the series of an episode's file by the show's name as the
-- folder (or the file) spells it. Enrichment then gives the series the
-- reference database's title, and its sort title follows: "Example- The Show"
-- becomes "Example: The Show". A file of the same folder scanned after that,
-- seconds later in the same scan or a week later, no longer found its series
-- by the sort title and made another one: one show, several series, its
-- episodes spread over them.
--
-- scantitle keeps the name the scanner looked the series up by (lower case),
-- set when the scanner makes the series and never changed after: the scanner
-- finds a series by it first, by the sort title as before for a series made
-- without it.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. Adding the
-- column rewrites nothing; the partial index holds the series alone.
ALTER TABLE com_nalet_katalog_items ADD COLUMN IF NOT EXISTS scantitle VARCHAR(500);
CREATE INDEX IF NOT EXISTS idx_items_series_scantitle ON com_nalet_katalog_items (scantitle)
  WHERE type = 'series' AND scantitle IS NOT NULL;
