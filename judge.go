package airut

import (
	"context"
	"errors"
	"time"
)

// Decision is what a judge decided.
type Decision string

// The decisions. A judgement that fails, times out, is refused, or comes
// back as anything else is an abstention, which a bot treats as no.
const (
	Yes     Decision = "yes"
	No      Decision = "no"
	Abstain Decision = "abstain"
)

// Judgement is one yes-or-no question for a small, fast model, such as
// whether to speak up in a conversation nobody asked the bot into. It
// is not an agent turn: it calls no tool, remembers nothing, and never
// draws on the Gate's rate.
type Judgement struct {
	// Persona names the judge's instructions, among the personas the
	// Judge was given.
	Persona string
	// Prompt is the question. It is the app's own text, never chat.
	Prompt string
	// Context is chat the judge reads before the batch, oldest first, as
	// quoted chat.
	Context []Quoted
	// Batch is the messages the question is about, oldest first. A yes
	// names one of them as its Target.
	Batch []Quoted
	// Key coalesces judgements: while one waits for the judge, a newer
	// one with the same key replaces it, and it abstains with
	// ErrSuperseded. Empty means the channel.
	Key string
	// InputTokens caps the whole request this judgement makes, its
	// instructions, chat, and reply together, below the judge's own cap. A
	// JudgeLimit honours it. Zero leaves the judge's.
	InputTokens int
}

// Verdict is a judge's answer.
type Verdict struct {
	Decision Decision
	// Target is the message of the batch a yes is about. It is zero for
	// any other decision, and can name only a message of the batch.
	Target MessageRef
	// Reason is the judge's own words. It is model text drawn from chat,
	// so a bot may show it but must never log it or follow it.
	Reason string
}

// Judge decides judgements. terva.sh/airut/agent implements it on a
// model; the core knows only this interface. It may return any decision
// or error: Ctx.Judge turns everything but a clean yes or no into an
// abstention.
type Judge interface {
	Judge(c *Ctx, j Judgement) (Verdict, error)
}

// JudgeLimit is a Judge that caps the request a judgement makes, such as
// to fit a model server's context slot. Chime asks it before each
// judgement, and drops the oldest half of its context until the
// judgement fits, so the prompt's opening changes only on a drop. The
// judge still enforces the cap on whatever it is given.
type JudgeLimit interface {
	Judge
	// InputTokens is the judge's own cap, in tokens, or zero for none.
	InputTokens() int
	// Fits reports whether j's whole request stays within the smaller of
	// InputTokens and j.InputTokens.
	Fits(j Judgement) bool
}

// JudgeRate caps the judgements one channel may ask for in any Window.
// Judgements are counted in memory, so a restart forgets them.
type JudgeRate struct {
	PerChannel int
	Window     time.Duration
}

var (
	// ErrNoJudge is the error of a judgement on a bot with no Judge.
	ErrNoJudge = errors.New("airut: the bot has no judge")
	// ErrJudgeRate is the error of a judgement over Config.JudgeRate.
	ErrJudgeRate = errors.New("airut: the channel has asked for too many judgements")
	// ErrSuperseded is the error of a judgement a newer one with the same
	// key replaced while it waited.
	ErrSuperseded = errors.New("airut: a newer judgement replaced this one")
)

// Judge asks the bot's Judge for a verdict. It always returns one: a
// failure, a timeout, a refusal by JudgeRate, or a reply that is not a
// clean yes or no comes back as Abstain, with the error that caused it
// when there was one. A yes whose Target is not a message of the batch
// abstains too.
//
// It counts airut_judge_total by persona and decision, and logs the
// invocation, the persona, and the decision: never the prompt, the
// chat, or the reason.
func (c *Ctx) Judge(j Judgement) (Verdict, error) {
	start := time.Now()
	if j.Key == "" {
		j.Key = string(c.inv.Channel.Platform) + "/channel/" + c.inv.Channel.ID
	}
	v, err := c.judge(j)
	if err != nil || (v.Decision != Yes && v.Decision != No) || !inBatch(v, j.Batch) {
		v = Verdict{Decision: Abstain}
	}
	if v.Decision != Yes {
		v.Target = MessageRef{}
	}
	m := c.bot.metrics
	m.Add("airut_judge_total", 1, "persona", j.Persona, "decision", string(v.Decision))
	m.Since("airut_judge_seconds", start, "persona", j.Persona)
	log := c.bot.cfg.Logger.Debug
	if err != nil && !errors.Is(err, ErrSuperseded) {
		log = c.bot.cfg.Logger.Info
	}
	log("judgement", "invocation", c.inv.ID, "persona", j.Persona, "decision", string(v.Decision),
		"batch", len(j.Batch), "error_kind", judgeErrorKind(err))
	return v, err
}

func (c *Ctx) judge(j Judgement) (Verdict, error) {
	if c.bot.cfg.Judge == nil {
		return Verdict{}, ErrNoJudge
	}
	if r := c.bot.cfg.JudgeRate; r.PerChannel > 0 {
		inv := Invocation{Caller: User{Platform: c.inv.Channel.Platform}, Channel: c.inv.Channel}
		if err := c.bot.judged.take(Gate{PerChannel: r.PerChannel, Window: r.Window}, inv); err != nil {
			return Verdict{}, ErrJudgeRate
		}
	}
	return c.bot.cfg.Judge.Judge(c, j)
}

// inBatch reports whether a yes names a message of batch, or batch is
// empty and it names none.
func inBatch(v Verdict, batch []Quoted) bool {
	if v.Decision != Yes {
		return true
	}
	if len(batch) == 0 {
		return v.Target == MessageRef{}
	}
	for _, q := range batch {
		if q.Ref == v.Target && q.Ref.ID != "" {
			return true
		}
	}
	return false
}

func judgeErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoJudge):
		return "no_judge"
	case errors.Is(err, ErrJudgeRate):
		return "rate"
	case errors.Is(err, ErrSuperseded):
		return "superseded"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "other"
}
