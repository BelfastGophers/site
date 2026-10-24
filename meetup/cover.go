package meetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

	"belfastgophers.com/site/store"
)

// SyncCover keeps the next event's cover in step with its page on Meetup,
// for the home page. The feed does not carry it. It is downloaded only when
// the event has none, or Meetup's has changed.
func SyncCover(ctx context.Context, client *http.Client, st *store.Store, now time.Time) error {
	next, err := st.NextEvent(ctx, now)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	source, err := Cover(ctx, client, next.URL)
	if err != nil || source == "" {
		return err
	}
	saved, err := st.CoverSource(ctx, next.ID)
	if err == nil && saved == source {
		return nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	image, err := Image(ctx, client, source)
	if err != nil {
		return err
	}
	return st.SaveCover(ctx, next.ID, source, image)
}

// Cover returns the address of an event's cover image, the og:image of its
// page on Meetup, or "" when the page has none.
func Cover(ctx context.Context, client *http.Client, eventURL string) (string, error) {
	page, err := get(ctx, client, eventURL)
	if err != nil {
		return "", fmt.Errorf("meetup event page: %w", err)
	}
	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return "", nil
			}
			return "", fmt.Errorf("meetup event page: %w", z.Err())
		case html.StartTagToken, html.SelfClosingTagToken:
			if source, ok := ogImage(z.Token()); ok {
				return source, nil
			}
		}
	}
}

// ogImage is the address in a <meta property="og:image"> tag.
func ogImage(t html.Token) (string, bool) {
	if t.Data != "meta" {
		return "", false
	}
	var property, content string
	for _, a := range t.Attr {
		switch a.Key {
		case "property":
			property = a.Val
		case "content":
			content = a.Val
		}
	}
	return content, property == "og:image" && content != ""
}

// Image downloads an image from Meetup, refusing anything that is not one.
func Image(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	image, err := get(ctx, client, source)
	if err != nil {
		return nil, fmt.Errorf("meetup image: %w", err)
	}
	if kind := http.DetectContentType(image); !strings.HasPrefix(kind, "image/") {
		return nil, fmt.Errorf("meetup image %s is %s, not an image", source, kind)
	}
	return image, nil
}
