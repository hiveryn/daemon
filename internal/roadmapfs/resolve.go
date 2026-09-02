package roadmapfs

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hiveryn/daemon/internal/domain"
)

// resolveTickets resolves the ticket IDs linked from items against the same
// architect board. Resolution is display-only enrichment and never fails a
// read or update: missing tickets and board-read failures degrade to
// warnings. Output is sorted by ticket ID for determinism.
func (s *Service) resolveTickets(ctx context.Context, architectPath string, items []domain.RoadmapItem) ([]domain.RoadmapTicketInfo, []string) {
	linkedBy := make(map[string]string) // ticket ID -> first linking item ID
	ticketIDs := []string{}
	for _, item := range items {
		for _, ticketID := range item.Tickets {
			if _, seen := linkedBy[ticketID]; seen {
				continue
			}
			linkedBy[ticketID] = item.ID
			ticketIDs = append(ticketIDs, ticketID)
		}
	}
	if len(ticketIDs) == 0 {
		return []domain.RoadmapTicketInfo{}, []string{}
	}
	sort.Strings(ticketIDs)

	board, err := s.tickets.ListTickets(ctx, architectPath)
	if err != nil {
		return []domain.RoadmapTicketInfo{}, []string{
			fmt.Sprintf("ticket resolution unavailable: list tickets: %v", err),
		}
	}
	summaries := make(map[string]domain.TicketSummary)
	for _, column := range [][]domain.TicketSummary{board.Backlog, board.Progress, board.Done} {
		for _, summary := range column {
			summaries[summary.ID] = summary
		}
	}

	infos := []domain.RoadmapTicketInfo{}
	warnings := []string{}
	for _, ticketID := range ticketIDs {
		summary, found := summaries[ticketID]
		if !found {
			warnings = append(warnings, fmt.Sprintf("ticket %q linked from roadmap item %q not found on this board", ticketID, linkedBy[ticketID]))
			continue
		}
		info := domain.RoadmapTicketInfo{
			ID:              summary.ID,
			Title:           summary.Title,
			Status:          summary.Status,
			Repo:            summary.Repo,
			AdditionalRepos: summary.AdditionalRepos,
			HasConclusion:   summary.HasConclusion,
		}
		if info.AdditionalRepos == nil {
			info.AdditionalRepos = []string{}
		}
		if summary.HasConclusion {
			ticket, err := s.tickets.GetTicket(ctx, architectPath, ticketID)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				// Deleted between the board scan and this read; report it the
				// same way as a missing link.
				warnings = append(warnings, fmt.Sprintf("ticket %q linked from roadmap item %q not found on this board", ticketID, linkedBy[ticketID]))
				continue
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("ticket %q: conclusion unavailable: %v", ticketID, err))
			case ticket.Conclusion != nil:
				info.ConclusionOutcome = ticket.Conclusion.Outcome
			}
		}
		infos = append(infos, info)
	}
	return infos, warnings
}
