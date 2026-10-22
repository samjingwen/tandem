package opencode

import (
	"context"
	"net/http"
	"net/url"

	"github.com/samjingwen/tandem/core"
)

type backgroundResource struct {
	Status    string `json:"status"`
	SessionID string `json:"sessionID"`
}

// CheckQuiescence complements foreground observations with the pinned runtime's
// known shell/PTY inventories. It never stops or adopts an unrelated resource.
// Workspace cleanup separately checks OS process cwd holders; neither is a claim
// of an operating-system sandbox or complete arbitrary-daemon containment.
func (r *Runtime) CheckQuiescence(ctx context.Context, spec core.SessionSpec) error {
	if err := r.owned(ctx, spec); err != nil {
		return err
	}
	observation, err := r.Inspect(ctx, spec)
	if err != nil {
		return err
	}
	if !observation.Exists || !observation.Owned || (observation.State != "idle" && observation.State != "failed") {
		return core.Invalid("runtime execution is not quiescent")
	}
	if len(observation.Decisions) > 0 {
		return core.Invalid("runtime still owns pending permissions or forms")
	}
	query := url.Values{}
	query.Set("location[directory]", spec.Directory)
	for _, path := range []string{"/api/shell", "/api/pty"} {
		var result struct {
			Location location              `json:"location"`
			Data     *[]backgroundResource `json:"data"`
		}
		if err = r.request(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &result); err != nil {
			return err
		}
		if result.Data == nil || result.Location.Directory != spec.Directory {
			return ErrOwnership
		}
		for _, resource := range *result.Data {
			if resource.Status == "running" {
				return core.Invalid("runtime location still owns a running shell or terminal")
			}
			if resource.Status != "exited" && resource.Status != "killed" && resource.Status != "timeout" {
				return core.ErrUncertain
			}
		}
	}
	var info struct {
		Capabilities struct {
			PersistentPTY bool `json:"persistentPty"`
		} `json:"capabilities"`
	}
	if err = r.request(ctx, http.MethodGet, "/api/info", nil, &info); err != nil {
		return err
	}
	if info.Capabilities.PersistentPTY {
		resources, err := getData[[]backgroundResource](r, ctx, "/api/experimental/session/"+url.PathEscape(spec.Reference)+"/terminal")
		if err != nil {
			return err
		}
		for _, resource := range resources {
			if resource.SessionID != spec.Reference {
				return ErrOwnership
			}
			if resource.Status == "running" {
				return core.Invalid("runtime session still owns a persistent terminal")
			}
			if resource.Status != "exited" {
				return core.ErrUncertain
			}
		}
	}
	return nil
}
