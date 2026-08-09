package workflow

import (
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
)

// RegisterActivities binds the runner implementation to stable activity names
// used by the workflows. The control plane can use the same helper in every
// worker process without exposing a container-runtime socket.
func RegisterActivities(w worker.Worker, implementation RunnerActivities) {
	handlers := ActivityHandlers{Impl: implementation}
	w.RegisterActivityWithOptions(handlers.ScheduleRunnerTask, activity.RegisterOptions{Name: ActivitySchedule})
	w.RegisterActivityWithOptions(handlers.CancelRunnerTask, activity.RegisterOptions{Name: ActivityCancel})
	w.RegisterActivityWithOptions(handlers.ResumeRunnerTask, activity.RegisterOptions{Name: ActivityResume})
	w.RegisterActivityWithOptions(handlers.StatusRunnerTask, activity.RegisterOptions{Name: ActivityStatus})
}
