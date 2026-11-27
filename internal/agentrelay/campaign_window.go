package agentrelay

import (
	"context"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// CampaignWindows lets the campaign worker queue measured windows on an
// agent and read them back. It implements optimizationcampaign.AgentWindows.
type CampaignWindows struct{ Service *Service }

// campaignActor is who queues a run's windows.
const campaignActor = "optimization-campaign"

func (c CampaignWindows) QueueWindow(ctx context.Context, tenant, agentID, key string, spec optimizationcampaign.AgentWindowSpec) (optimizationcampaign.AgentWindow, error) {
	command, err := c.Service.QueueWindow(ctx, tenant, agentID, campaignActor, key, spec)
	if err != nil {
		return optimizationcampaign.AgentWindow{}, err
	}
	window := optimizationcampaign.AgentWindow{ID: command.ID, State: command.State, FailureCode: command.FailureCode, FailureDetail: command.FailureDetail, StartedAt: command.StartedAt, FinishedAt: command.FinishedAt}
	if _, result, decodeErr := DecodeWindow(command); decodeErr == nil {
		window.Result = result
	}
	return window, nil
}

func (c CampaignWindows) CancelWindow(ctx context.Context, tenant, agentID, id string) error {
	_, err := c.Service.CancelWindow(ctx, tenant, agentID, id)
	return err
}

// QueueServerCommand implements optimizationcampaign.AgentServerCommands.
func (c CampaignWindows) QueueServerCommand(ctx context.Context, tenant, agentID, key string, start *agentprotocol.StartServerSpec, stop *agentprotocol.StopServerSpec) (optimizationcampaign.AgentServerCommand, error) {
	command, err := c.Service.QueueServerCommand(ctx, tenant, agentID, campaignActor, key, start, stop)
	if err != nil {
		return optimizationcampaign.AgentServerCommand{}, err
	}
	out := optimizationcampaign.AgentServerCommand{ID: command.ID, State: command.State, FailureCode: command.FailureCode, FailureDetail: command.FailureDetail}
	if result, decodeErr := DecodeServerResult(command); decodeErr == nil {
		out.Result = result
	}
	return out, nil
}
