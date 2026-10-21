package store

import (
	"context"
	"fmt"
	"time"
)

// Proposal is a talk someone has offered to give.
type Proposal struct {
	ID              int64
	Name            string
	Email           string
	Title           string
	Description     string
	DurationMinutes int
	Remote          bool
	SubmittedAt     time.Time
}

// SubmitProposal saves a proposal and returns its id.
func (s *Store) SubmitProposal(ctx context.Context, p Proposal) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO talk_proposals (name, email, title, description, duration_minutes, remote, submitted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Email, p.Title, p.Description, p.DurationMinutes, p.Remote, format(p.SubmittedAt))
	if err != nil {
		return 0, fmt.Errorf("submit proposal: %w", err)
	}
	return res.LastInsertId()
}

// Proposals returns every talk proposal, newest first.
func (s *Store) Proposals(ctx context.Context) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, email, title, description, duration_minutes, remote, submitted_at
		 FROM talk_proposals ORDER BY submitted_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("query proposals: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err has the error that matters
	var proposals []Proposal
	for rows.Next() {
		var p Proposal
		var submitted string
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &p.Title, &p.Description, &p.DurationMinutes, &p.Remote, &submitted); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		if p.SubmittedAt, err = parse(submitted); err != nil {
			return nil, err
		}
		proposals = append(proposals, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query proposals: %w", err)
	}
	return proposals, nil
}
