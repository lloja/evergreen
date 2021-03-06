package service

import (
	"fmt"
	"net/http"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/alerts"
	"github.com/evergreen-ci/evergreen/apimodels"
	"github.com/evergreen-ci/evergreen/bookkeeping"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/taskrunner"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

// markHostRunningTaskFinished updates the running task field in the host document.
// TODO: this should be taken out when the task runner no longer assigns tasks to the agent. (EVG-1591)
func markHostRunningTaskFinished(h *host.Host, t *task.Task, newTaskId string) {
	// clear the running task instead
	if newTaskId == "" {
		err := h.ClearRunningTask(t.Id, time.Now())
		if err != nil {
			grip.Errorf("error clearing task %s on host %s : %+v", t.Id, h.Id, err)
			return
		}
	}
	// update the given host's running_task field accordingly
	if ok, err := h.UpdateRunningTask(t.Id, newTaskId, time.Now()); err != nil || !ok {
		grip.Errorf("%s on host %s to '': %+v", t.Id, h.Id, err)
	}
}

// taskFinished constructs the appropriate response for each markEnd
// request the API server receives from an agent. The two possible responses are:
// 1. Inform the agent of another task to run
// 2. Inform the agent that it should terminate immediately
// The first case is the usual expected flow. The second case however, could
// occur for a number of reasons including:
// a. The version of the agent running on the remote machine is stale
// b. The host the agent is running on has been decommissioned
// c. There is no currently queued dispatchable and activated task
// In any of these aforementioned cases, the agent in question should terminate
// immediately and cease running any tasks on its host.
func (as *APIServer) taskFinished(w http.ResponseWriter, t *task.Task, finishTime time.Time) {
	taskEndResponse := &apimodels.TaskEndResponse{}

	// a. fetch the host this task just completed on to see if it's
	// now decommissioned
	host, err := host.FindOne(host.ByRunningTaskId(t.Id))
	if err != nil {
		message := fmt.Sprintf("Error locating host for task %v - set to %v: %v", t.Id,
			t.HostId, err)
		grip.Error(message)
		taskEndResponse.Message = message
		as.WriteJSON(w, http.StatusInternalServerError, taskEndResponse)
		return
	}
	if host == nil {
		message := fmt.Sprintf("Error finding host running for task %v - set to %v", t.Id,
			t.HostId)
		grip.Error(message)
		taskEndResponse.Message = message
		as.WriteJSON(w, http.StatusInternalServerError, taskEndResponse)
		return
	}
	if host.Status == evergreen.HostDecommissioned || host.Status == evergreen.HostQuarantined {
		markHostRunningTaskFinished(host, t, "")
		message := fmt.Sprintf("Host %v - running %v - is in state '%v'. Agent will terminate",
			t.HostId, t.Id, host.Status)
		grip.Info(message)
		taskEndResponse.Message = message
		as.WriteJSON(w, http.StatusOK, taskEndResponse)
		return
	}

	// task cost calculations have no impact on task results, so do them in their own goroutine
	go as.updateTaskCost(t, host, finishTime)

	// b. check if the agent needs to be rebuilt
	taskRunnerInstance := taskrunner.NewTaskRunner(&as.Settings)
	agentRevision, err := taskRunnerInstance.HostGateway.GetAgentRevision()
	if err != nil {
		markHostRunningTaskFinished(host, t, "")
		grip.Errorln("failed to get agent revision:", err)
		taskEndResponse.Message = err.Error()
		as.WriteJSON(w, http.StatusInternalServerError, taskEndResponse)
		return
	}
	if host.AgentRevision != agentRevision {
		markHostRunningTaskFinished(host, t, "")
		message := fmt.Sprintf("Remote agent needs to be rebuilt")
		grip.Error(message)
		taskEndResponse.Message = message
		as.WriteJSON(w, http.StatusOK, taskEndResponse)
		return
	}

	// c. fetch the task's distro queue to dispatch the next pending task
	nextTask, err := getNextDistroTask(t, host)
	if err != nil {
		markHostRunningTaskFinished(host, t, "")
		grip.Error(err)
		taskEndResponse.Message = err.Error()
		as.WriteJSON(w, http.StatusOK, taskEndResponse)
		return
	}
	if nextTask == nil {
		markHostRunningTaskFinished(host, t, "")
		taskEndResponse.Message = "No next task on queue"
	} else {
		taskEndResponse.Message = "Proceed with next task"
		taskEndResponse.RunNext = true
		taskEndResponse.TaskId = nextTask.Id
		taskEndResponse.TaskSecret = nextTask.Secret
		markHostRunningTaskFinished(host, t, nextTask.Id)
	}

	// give the agent the green light to keep churning
	as.WriteJSON(w, http.StatusOK, taskEndResponse)
}

// getNextDistroTask fetches the next task to run for the given distro and marks
// the task as dispatched in the given host's document
func getNextDistroTask(currentTask *task.Task, host *host.Host) (
	nextTask *task.Task, err error) {
	taskQueue, err := model.FindTaskQueueForDistro(currentTask.DistroId)
	if err != nil {
		return nil, errors.Wrapf(err, "Error locating distro queue (%v) for task '%v'",
			currentTask.DistroId, currentTask.Id)
	}

	if taskQueue == nil {
		return nil, errors.Errorf("Nil task queue found for task '%v's distro "+
			"queue - '%v'", currentTask.Id, currentTask.DistroId)
	}

	// dispatch the next task for this host
	nextTask, err = taskrunner.DispatchTaskForHost(taskQueue, host)
	if err != nil {
		return nil, errors.Wrapf(err, "Error dequeuing task for host %v", host.Id)
	}
	if nextTask == nil {
		return nil, nil
	}
	return nextTask, nil
}

// EndTask creates test results from the request and the project config.
// It then acquires the lock, and with it, marks tasks as finished or inactive if aborted.
// If the task is a patch, it will alert the users based on failures
// It also updates the expected task duration of the task for scheduling.
func (as *APIServer) oldEndTask(w http.ResponseWriter, r *http.Request) {
	finishTime := time.Now()
	taskEndResponse := &apimodels.TaskEndResponse{}

	t := MustHaveTask(r)

	details := &apimodels.TaskEndDetail{}
	if err := util.ReadJSONInto(util.NewRequestReader(r), details); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Check that finishing status is a valid constant
	if details.Status != evergreen.TaskSucceeded &&
		details.Status != evergreen.TaskFailed &&
		details.Status != evergreen.TaskUndispatched {
		msg := errors.Errorf("Invalid end status '%v' for task %v", details.Status, t.Id)
		as.LoggedError(w, r, http.StatusBadRequest, msg)
		return
	}

	projectRef, err := model.FindOneProjectRef(t.Project)

	if err != nil {
		as.LoggedError(w, r, http.StatusInternalServerError, err)
	}

	if projectRef == nil {
		as.LoggedError(w, r, http.StatusNotFound, errors.New("empty projectRef for task"))
		return
	}

	project, err := model.FindProject("", projectRef)
	if err != nil {
		as.LoggedError(w, r, http.StatusInternalServerError, errors.WithStack(err))
		return
	}

	if !getGlobalLock(r.RemoteAddr, t.Id, EndTaskCaller) {
		as.LoggedError(w, r, http.StatusInternalServerError, ErrLockTimeout)
		return
	}
	defer releaseGlobalLock(r.RemoteAddr, t.Id, EndTaskCaller)

	// mark task as finished
	err = model.MarkEnd(t.Id, APIServerLockTitle, finishTime, details, project, projectRef.DeactivatePrevious)
	if err != nil {
		message := errors.Wrapf(err, "Error calling mark finish on task %v", t.Id)
		as.LoggedError(w, r, http.StatusInternalServerError, message)
		return
	}

	// TODO(EVG-223) process patch-specific triggers
	if t.Requester != evergreen.PatchVersionRequester {
		grip.Infoln("Processing alert triggers for task", t.Id)
		err = errors.WithStack(alerts.RunTaskFailureTriggers(t.Id))
		grip.ErrorWhenf(err != nil, "processing alert triggers for task %s: %+v", t.Id, err)
	}

	// if task was aborted, reset to inactive
	if details.Status == evergreen.TaskUndispatched {
		if err = model.SetActiveState(t.Id, "", false); err != nil {
			message := fmt.Sprintf("Error deactivating task after abort: %v", err)
			grip.Error(message)
			taskEndResponse.Message = message
			as.WriteJSON(w, http.StatusInternalServerError, taskEndResponse)
			return
		}

		as.taskFinished(w, t, finishTime)
		return
	}

	// update the bookkeeping entry for the task
	err = bookkeeping.UpdateExpectedDuration(t, t.TimeTaken)
	if err != nil {
		grip.Errorln("Error updating expected duration:", err.Error())
	}

	// log the task as finished
	grip.Infof("Successfully marked task %s as finished", t.Id)

	// construct and return the appropriate response for the agent
	as.taskFinished(w, t, finishTime)
}

// StartTask is the handler function that retrieves the task from the request
// and acquires the global lock
// With the lock, it marks associated tasks, builds, and versions as started.
// It then updates the host document with relevant information, including the pid
// of the agent, and ensures that the host has the running task field set.
func (as *APIServer) oldStartTask(w http.ResponseWriter, r *http.Request) {
	t := MustHaveTask(r)

	if !getGlobalLock(r.RemoteAddr, t.Id, TaskStartCaller) {
		as.LoggedError(w, r, http.StatusInternalServerError, ErrLockTimeout)
		return
	}
	defer releaseGlobalLock(r.RemoteAddr, t.Id, TaskStartCaller)

	grip.Infoln("Marking task started:", t.Id)

	taskStartInfo := &apimodels.TaskStartRequest{}
	if err := util.ReadJSONInto(util.NewRequestReader(r), taskStartInfo); err != nil {
		http.Error(w, fmt.Sprintf("Error reading task start request for %v: %v", t.Id, err), http.StatusBadRequest)
		return
	}

	if err := model.MarkStart(t.Id); err != nil {
		message := errors.Wrapf(err, "Error marking task '%s' started", t.Id)
		as.LoggedError(w, r, http.StatusInternalServerError, message)
		return
	}

	h, err := host.FindOne(host.ByRunningTaskId(t.Id))
	if err != nil {
		message := errors.Wrapf(err, "Error finding host running task %s", t.Id)
		as.LoggedError(w, r, http.StatusInternalServerError, message)
		return
	}

	if h == nil {
		message := errors.Errorf("No host found running task %v", t.Id)
		if t.HostId != "" {
			message = errors.Errorf("No host found running task %s but task is said to be running on %s",
				t.Id, t.HostId)
		}

		as.LoggedError(w, r, http.StatusInternalServerError, message)
		return
	}

	if err := h.SetTaskPid(taskStartInfo.Pid); err != nil {
		message := errors.Wrapf(err, "Error calling set pid on task %s", t.Id)
		as.LoggedError(w, r, http.StatusInternalServerError, message)
		return
	}
	as.WriteJSON(w, http.StatusOK, fmt.Sprintf("Task %v started on host %v", t.Id, h.Id))
}
