-- Ticket workers may request Actions too (trigger worker). requester_ticket_id
-- names the requesting worker's ticket so approval history attributes it; it
-- stays empty for manual launches and architect requests.
ALTER TABLE action_runs ADD COLUMN requester_ticket_id TEXT;
