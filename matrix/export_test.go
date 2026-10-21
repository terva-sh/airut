package matrix

import "time"

// SetRelationPage sets how many reactions a duplicate-reaction lookup
// reads a request, and returns a function that restores it.
func SetRelationPage(n int) (restore func()) {
	old := relationPage
	relationPage = n
	return func() { relationPage = old }
}

// SetDirectReload sets how old the cached m.direct may grow, and returns
// a function that restores it.
func SetDirectReload(d time.Duration) (restore func()) {
	old := directReload
	directReload = d
	return func() { directReload = old }
}

// SetEditPage sets how many edits a lookup of a message's newest edit
// reads a request, and returns a function that restores it.
func SetEditPage(n int) (restore func()) {
	old := editPage
	editPage = n
	return func() { editPage = old }
}
