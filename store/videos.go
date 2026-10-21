package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Video is a talk on the group's YouTube channel.
type Video struct {
	YouTubeID   string
	Title       string
	PublishedAt time.Time
}

// SaveVideos adds the videos, or updates them by their YouTube id. Videos
// already saved and not among them are kept.
func (s *Store) SaveVideos(ctx context.Context, videos []Video) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save videos: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	for _, v := range videos {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO videos (youtube_id, title, published_at) VALUES (?, ?, ?)
			 ON CONFLICT (youtube_id) DO UPDATE SET title = excluded.title, published_at = excluded.published_at`,
			v.YouTubeID, v.Title, format(v.PublishedAt)); err != nil {
			return fmt.Errorf("save video %s: %w", v.YouTubeID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("save videos: %w", err)
	}
	return nil
}

// Videos returns every video, newest first.
func (s *Store) Videos(ctx context.Context) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT youtube_id, title, published_at FROM videos ORDER BY published_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query videos: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err has the error that matters
	var videos []Video
	for rows.Next() {
		var v Video
		var published string
		if err := rows.Scan(&v.YouTubeID, &v.Title, &published); err != nil {
			return nil, fmt.Errorf("scan video: %w", err)
		}
		if v.PublishedAt, err = parse(published); err != nil {
			return nil, err
		}
		videos = append(videos, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query videos: %w", err)
	}
	return videos, nil
}
