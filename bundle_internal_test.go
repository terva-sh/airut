package airut

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// askOnly is a Responder with no BundleAsker: every method but Ask is
// left to the nil interface, which the fallback never calls.
type askOnly struct {
	Responder
	asked []Ask
}

func (r *askOnly) Ask(_ context.Context, a Ask) (string, error) {
	r.asked = append(r.asked, a)
	return a.Choices[len(a.Choices)-1], nil
}

func TestAskAllOneAtATime(t *testing.T) {
	b := New(Config{})
	r := &askOnly{}
	caller := User{Platform: "test", ID: "u1"}
	c := &Ctx{ctx: context.Background(), bot: b, inv: Invocation{Caller: caller}, resp: r}
	got, err := c.AskAll(Bundle{
		Intro: "Two things.",
		Questions: []Question{
			{Key: "post", Text: "Which post?", Choices: []string{"troll", "tolkien"}},
			{Key: "keep", Text: "Keep the rating?", Choices: []string{"yes", "no"}},
		},
		Who: func(Answerer) bool { return true },
	})
	if want := map[string]Answer{"post": {Text: "tolkien", By: caller}, "keep": {Text: "no", By: caller}}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("AskAll = %v, %v", got, err)
	}
	if len(r.asked) != 2 || r.asked[0].Question != "Two things.\n\n1. Which post?" || r.asked[1].Question != "2. Keep the rating?" {
		t.Fatalf("asked %+v", r.asked)
	}
	// Ask cannot admit anyone else, so a widened Who still puts each
	// question to the caller alone.
	for _, a := range r.asked {
		if a.Who != caller {
			t.Errorf("a question went to %+v", a.Who)
		}
	}

	r.asked = nil
	_, err = c.AskAll(Bundle{Questions: []Question{{Key: "a", Text: "Yes?", Choices: []string{"yes", "no"}}, {Key: "b", Text: "Why?"}}})
	if err == nil || !strings.Contains(err.Error(), "free-text") || len(r.asked) != 0 {
		t.Fatalf("a free-text question without a BundleAsker: %v, after asking %d", err, len(r.asked))
	}
	if errors.Is(err, ErrAskTimeout) {
		t.Fatal("refusing a bundle is not a timeout")
	}
}
