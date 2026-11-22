package daemon

import (
	"github.com/abanna/herdr-agentisan/internal/attention"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

func AttentionConfigForTest(s settings.NoticeSettings, project string) attention.Config {
	return newAttentionConfigs(s).For(project)
}

func AttentionStateForTest(p store.BoardPending, m *team.Model, project, boss string) attention.State {
	return attentionStateOf(p, m, project, boss)
}
