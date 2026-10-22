//go:build !darwin && !linux

package workspace

import (
	"context"
	"errors"
)

func refuseProcessHolders(context.Context, string) error {
	return errors.New("workspace cleanup process verification is supported on macOS/Linux only")
}
