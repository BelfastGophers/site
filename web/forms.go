package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the site's zone, wherever it runs
	"unicode/utf8"
)

// zone is where the meet-ups are: event times are entered and shown in it.
var zone = func() *time.Location {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		panic(err) // time/tzdata is linked in, so it is always there
	}
	return loc
}()

// fieldErrors is what is wrong with a form, a message for each field by the
// name the form sends it under. A form's Validate method returns one, which
// binder.Bind returns wrapped, and the form is shown again with them.
type fieldErrors map[string]string

func (f fieldErrors) Error() string { return fmt.Sprintf("%d fields are not filled in right", len(f)) }

// text records a message for a field that is blank or longer than limit.
func (f fieldErrors) text(field, value string, limit int) {
	switch {
	case strings.TrimSpace(value) == "":
		f[field] = "Please fill this in."
	case utf8.RuneCountInString(value) > limit:
		f[field] = "Please keep this to " + strconv.Itoa(limit) + " characters."
	}
}

// err is f as Validate's result: nil when nothing is wrong.
func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// eventPath is the {id} in an event's path.
type eventPath struct {
	ID int64 `path:"id,required"`
}
