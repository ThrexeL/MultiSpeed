package main

import (
	"context"
	"errors"
	"time"

	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/route"
	"github.com/multispeed-app/multispeed/internal/tun"
)

// activeSession owns the resources of one connected tunnel.
type activeSession struct {
	ctx     context.Context
	client  *client.Client
	dev     tun.Device
	routes  *route.Manager
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}

// runSession pumps the tunnel until it stops, then releases resources and
// restores the host's previous routing state.
func (a *App) runSession(session *activeSession) {
	err := session.client.Run(session.ctx, session.dev)
	stopped := err == nil || errors.Is(err, context.Canceled)
	if !stopped {
		a.logger.Error("session ended", "error", err)
	}

	if closeErr := session.routes.Close(); closeErr != nil {
		a.logger.Warn("could not restore system routes", "error", closeErr)
	}
	_ = session.dev.Close()
	_ = session.client.Close()
	close(session.done)

	a.mu.Lock()
	if a.session == session {
		a.session = nil
		a.state = "disconnected"
		if !stopped {
			a.lastErr = err.Error()
		}
	}
	a.mu.Unlock()

	if stopped {
		a.logger.Info("disconnected")
	}
}
