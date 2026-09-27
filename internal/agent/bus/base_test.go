package bus

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

// The base is what the bot published last: a card changed since is read in
// its new version, a removed card is gone, and only rejected drafts count as
// rejected.
func TestReadBase(t *testing.T) {
	b := newBench(t)
	s := b.survey.Contract()
	b.produce(t, contract.TopicSurvey, contract.SurveyKey, &s)
	gone := knowledge.Card{ID: "gone", Categories: []string{"leisure"}, Title: "Было", Summary: "s"}
	for _, c := range []knowledge.Card{transport, museum, gone} {
		m := c.Contract()
		b.produce(t, contract.TopicCards, c.ID, &m)
	}
	newer := transport
	newer.Summary = "Проездной подорожал."
	m := newer.Contract()
	b.produce(t, contract.TopicCards, newer.ID, &m)
	b.produce(t, contract.TopicCards, gone.ID, nil)
	b.produce(t, contract.TopicDecisions, "d1",
		&contract.Decision{DraftID: "d1", CardID: "transport", Verdict: contract.VerdictRejected, DecidedAt: at})
	b.produce(t, contract.TopicDecisions, "d2",
		&contract.Decision{DraftID: "d2", CardID: "museums_free", Verdict: contract.VerdictApproved, DecidedAt: at})

	base, err := ReadBase(context.Background(), b.brokers, b.codec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.Survey, b.survey) {
		t.Error("the survey changed on the way")
	}
	if !reflect.DeepEqual(base.Cards, []knowledge.Card{museum, newer}) {
		t.Errorf("cards %+v", base.Cards)
	}
	if !reflect.DeepEqual(base.Rejected, map[string]bool{"d1": true}) {
		t.Errorf("rejected %v", base.Rejected)
	}
}

// Without the survey the agent can't check its cards, so it waits for the
// bot.
func TestReadBaseBeforeTheBot(t *testing.T) {
	b := newBench(t)
	m := transport.Contract()
	b.produce(t, contract.TopicCards, transport.ID, &m)
	if _, err := ReadBase(context.Background(), b.brokers, b.codec); !errors.Is(err, ErrNoBase) {
		t.Errorf("got %v", err)
	}
}
