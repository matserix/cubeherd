package manager

import (
	"context"

	"cubeherd/internal/server"
)

// Perf reads the server's tick performance over RCON (one call per command, ~6 s timeout each).
// The caller must only ask a running server: RCON does not wake a paused one.
func (m *Manager) Perf(ctx context.Context, s *server.Server) (server.PerfSample, error) {
	var outs []string
	for _, c := range server.PerfCommands(s.Type) {
		out, err := m.RCON(ctx, s, c)
		if err != nil {
			return server.PerfSample{}, err
		}
		outs = append(outs, out)
	}
	return server.ParsePerf(s.Type, outs)
}
