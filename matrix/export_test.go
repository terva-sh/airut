package matrix

// SetRelationPage sets how many reactions a duplicate-reaction lookup
// reads a request, and returns a function that restores it.
func SetRelationPage(n int) (restore func()) {
	old := relationPage
	relationPage = n
	return func() { relationPage = old }
}
