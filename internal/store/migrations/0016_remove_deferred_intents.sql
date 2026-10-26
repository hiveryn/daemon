-- Deferred (manual) intents and their approval inputs existed only for the
-- removed executeAction tool; every remaining intent is blocking and lives in
-- memory and the session event log. Drop the durable record table.
DROP TABLE deferred_intents;
