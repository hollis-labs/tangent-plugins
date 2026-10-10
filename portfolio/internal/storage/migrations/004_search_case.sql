-- Rebuild the derived FTS projection with full Unicode lowercasing. Historical
-- JSON and receipts remain unchanged; old simple-rune lowercasing lost dotted-I
-- and contextual Greek matches. The normal trigger maintains the same index.
UPDATE items SET data=data;
