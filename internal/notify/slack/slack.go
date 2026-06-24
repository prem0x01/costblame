// Package slack delivers blame alerts to a Slack channel.
package slack

import (
	"context"
	"fmt"

	slackapi "github.com/slack-go/slack"

	"github.com/prem0x01/costblame/pkg/models"
)

// Notifier posts blame graph alerts to a Slack channel using the Slack Web API.
type Notifier struct {
	client    *slackapi.Client
	channel   string
	minScore  float64
}

// New creates a Slack Notifier.
// minScore is the confidence threshold below which alerts are suppressed.
func New(botToken, channel string, minScore float64) *Notifier {
	return &Notifier{
		client:   slackapi.New(botToken),
		channel:  channel,
		minScore: minScore,
	}
}

// Send posts a formatted blame alert. It is a no-op when the top blame confidence
// is below the configured minimum or when the graph has no edges.
func (n *Notifier) Send(ctx context.Context, graph models.BlameGraph) error {
	if graph.TopBlame == nil || graph.TopBlame.ConfidenceScore < n.minScore {
		return nil
	}

	top := graph.TopBlame
	anomaly := graph.Anomaly
	deploy := top.DeployEvent

	header := slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject("mrkdwn",
			fmt.Sprintf(":rotating_light: *Cost spike detected: %s* (+%.1f%%)",
				anomaly.Service, anomaly.DeltaPct), false, false),
		nil, nil,
	)

	var narrative string
	if top.Narrative != "" {
		narrative = top.Narrative
	} else {
		narrative = fmt.Sprintf("Cost increased %.1f%% on %s.",
			anomaly.DeltaPct, anomaly.PeriodStart.Format("Jan 2"))
	}

	body := slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject("mrkdwn", narrative, false, false),
		nil, nil,
	)

	var deployInfo string
	if deploy != nil {
		deployInfo = fmt.Sprintf("*PR #%d* — %s\n*Author:* @%s | *Confidence:* %.0f%%",
			deploy.PRNumber, deploy.PRTitle, deploy.PRAuthor, top.ConfidenceScore*100)
	}

	fields := slackapi.NewSectionBlock(nil, []*slackapi.TextBlockObject{
		slackapi.NewTextBlockObject("mrkdwn", "*Service*\n"+anomaly.Service, false, false),
		slackapi.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("*Amount*\n$%.2f (was $%.2f)", anomaly.AmountUSD, anomaly.PrevAmountUSD),
			false, false),
		slackapi.NewTextBlockObject("mrkdwn", "*Period*\n"+anomaly.PeriodStart.Format("Jan 2 15:04 UTC"), false, false),
		slackapi.NewTextBlockObject("mrkdwn", "*Deployment*\n"+deployInfo, false, false),
	}, nil)

	divider := slackapi.NewDividerBlock()

	actions := slackapi.NewActionBlock("blame_actions",
		slackapi.NewButtonBlockElement("confirm_blame", top.ID.String(),
			slackapi.NewTextBlockObject("plain_text", "Confirm", false, false)),
		slackapi.NewButtonBlockElement("dismiss_blame", top.ID.String(),
			slackapi.NewTextBlockObject("plain_text", "Dismiss", false, false)),
	)

	_, _, err := n.client.PostMessageContext(ctx, n.channel,
		slackapi.MsgOptionBlocks(header, body, fields, divider, actions),
	)
	if err != nil {
		return fmt.Errorf("slack: posting message: %w", err)
	}

	return nil
}
