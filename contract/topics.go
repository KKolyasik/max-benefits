package contract

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
)

// The topics and what goes through them.
const (
	// TopicDrafts carries Draft from the agent to the bot, keyed by the
	// draft ID.
	TopicDrafts = "knowledge.drafts"
	// TopicCards carries Card from the bot to the agent, keyed by the card
	// ID: the latest version of every card in the base.
	TopicCards = "knowledge.cards"
	// TopicSurvey carries Survey from the bot to the agent under SurveyKey.
	TopicSurvey = "knowledge.survey"
	// TopicDecisions carries Decision from the bot to the agent, keyed by
	// the draft ID.
	TopicDecisions = "knowledge.decisions"
	// TopicCommands carries RunCommand from the bot to the agent, keyed by
	// the command ID.
	TopicCommands = "agent.commands"
	// TopicRuns carries RunReport from the agent to the bot, keyed by the
	// run ID.
	TopicRuns = "agent.runs"
)

// Topic is a Kafka topic with its settings.
type Topic struct {
	Name    string
	Configs map[string]string
}

var compacted = map[string]string{"cleanup.policy": "compact"}

func retention(d time.Duration) map[string]string {
	return map[string]string{"retention.ms": strconv.FormatInt(d.Milliseconds(), 10)}
}

const day = 24 * time.Hour

// Topics are the topics of the contract. Compacted topics keep the latest
// record of every key for good: the agent reads them from the start to know
// the base, the survey and the decisions.
var Topics = []Topic{
	// The bot takes drafts at once; a month covers its downtime.
	{TopicDrafts, retention(30 * day)},
	{TopicCards, compacted},
	{TopicSurvey, compacted},
	{TopicDecisions, compacted},
	// A command is stale after a day.
	{TopicCommands, retention(day)},
	{TopicRuns, retention(30 * day)},
}

// EnsureTopics creates the topics that don't exist yet and leaves the rest
// as they are. One partition keeps the order of the records, and one replica
// is all a single broker has.
func EnsureTopics(ctx context.Context, adm *kadm.Client, topics ...Topic) error {
	for _, t := range topics {
		configs := make(map[string]*string, len(t.Configs))
		for k, v := range t.Configs {
			configs[k] = kadm.StringPtr(v)
		}
		resp, err := adm.CreateTopic(ctx, 1, 1, configs, t.Name)
		if err == nil {
			err = resp.Err
		}
		if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("create topic %s: %w", t.Name, err)
		}
	}
	return nil
}
