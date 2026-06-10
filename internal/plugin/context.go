package plugin

import (
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/tabplugin"
)

func BuildSessionContext(intent domain.SessionIntent, arch config.ArchitectConfig, ticket *domain.Ticket) tabplugin.SessionContext {
	return tabplugin.SessionContext{
		SessionID:      intent.ID,
		SessionType:    intent.SessionType,
		ArchitectKey:   intent.ArchitectKey,
		ArchitectPath:  arch.Path,
		ArchitectRepos: cloneStringMap(arch.Repos),
		Ticket:         ticket,
	}
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
