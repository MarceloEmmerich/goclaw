package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// makeCronJobHandler creates a cron job handler that routes through the scheduler's cron lane.
// This ensures per-session concurrency control (same job can't run concurrently)
// and integration with /stop, /stopall commands.
func makeCronJobHandler(sched *scheduler.Scheduler, msgBus *bus.MessageBus, cfg *config.Config, channelMgr *channels.Manager) func(job *store.CronJob) (*store.CronJobResult, error) {
	return func(job *store.CronJob) (*store.CronJobResult, error) {
		peerKind := resolveCronPeerKind(job)

		// Direct delivery: send Payload.Message straight to the channel without agent processing.
		// This is the correct behavior for reminders/notifications — bot sends one message directly.
		if job.Payload.Deliver && job.Payload.Channel != "" && job.Payload.To != "" {
			outMsg := bus.OutboundMessage{
				Channel: job.Payload.Channel,
				ChatID:  job.Payload.To,
				Content: job.Payload.Message,
			}
			if peerKind == "group" {
				outMsg.Metadata = map[string]string{"group_id": job.Payload.To}
			}
			msgBus.PublishOutbound(outMsg)

			return &store.CronJobResult{Content: job.Payload.Message}, nil
		}

		// Agent processing: route through scheduler for LLM-powered cron tasks.
		agentID := job.AgentID
		if agentID == "" {
			agentID = cfg.ResolveDefaultAgentID()
		} else {
			agentID = config.NormalizeAgentID(agentID)
		}

		sessionKey := sessions.BuildCronSessionKey(agentID, job.ID)
		channel := job.Payload.Channel
		if channel == "" {
			channel = "cron"
		}

		channelType := resolveChannelType(channelMgr, channel)

		outCh := sched.Schedule(context.Background(), scheduler.LaneCron, agent.RunRequest{
			SessionKey:  sessionKey,
			Message:     job.Payload.Message,
			Channel:     channel,
			ChannelType: channelType,
			ChatID:      job.Payload.To,
			PeerKind:    peerKind,
			UserID:      job.UserID,
			RunID:       fmt.Sprintf("cron:%s", job.ID),
			Stream:      false,
			TraceName:   fmt.Sprintf("Cron [%s] - %s", job.Name, agentID),
			TraceTags:   []string{"cron"},
		})

		outcome := <-outCh
		if outcome.Err != nil {
			return nil, outcome.Err
		}

		result := outcome.Result
		cronResult := &store.CronJobResult{
			Content: result.Content,
		}
		if result.Usage != nil {
			cronResult.InputTokens = result.Usage.PromptTokens
			cronResult.OutputTokens = result.Usage.CompletionTokens
		}

		return cronResult, nil
	}
}

// resolveCronPeerKind infers peer kind from the cron job's user ID.
// Group cron jobs have userID prefixed with "group:" (set during job creation).
func resolveCronPeerKind(job *store.CronJob) string {
	if strings.HasPrefix(job.UserID, "group:") {
		return "group"
	}
	return ""
}
