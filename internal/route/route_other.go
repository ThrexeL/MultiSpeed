//go:build !windows && !linux

package route

import (
	"context"
	"errors"
)

func apply(context.Context, Config, Runner) (*Manager, error) {
	return nil, errors.New("route: automatic route configuration is not supported on this platform")
}
