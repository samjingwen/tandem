package testruntime_test

import (
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/testruntime"
)

func TestInterruptRetainsHistoricalFailure(t *testing.T) {
	for _, state := range []string{"idle", "running", "failed"} {
		for _, action := range []string{"interrupt", "cancel"} {
			t.Run(state+"/"+action, func(t *testing.T) {
				runtime := testruntime.New()
				spec := core.SessionSpec{Reference: "session", TaskID: "task", AttemptID: "attempt", InstanceID: "instance", Directory: "/fixture"}
				if err := runtime.Ensure(t.Context(), spec); err != nil {
					t.Fatal(err)
				}
				runtime.SetState(spec.Reference, state)
				interrupt := runtime.Interrupt
				if action == "cancel" {
					interrupt = runtime.CancelPending
				}
				if err := interrupt(t.Context(), spec); err != nil {
					t.Fatal(err)
				}
				observation, err := runtime.Inspect(t.Context(), spec)
				want := "idle"
				if state == "failed" {
					want = "failed"
				}
				if err != nil || !observation.Exists || !observation.Owned || observation.State != want {
					t.Fatalf("interruption must park active execution without clearing historical failure: %+v, %v", observation, err)
				}
			})
		}
	}
}
