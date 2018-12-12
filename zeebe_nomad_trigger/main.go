package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	nomadAPI "github.com/hashicorp/nomad/api"
	"github.com/zeebe-io/zeebe/clients/go/entities"
	"github.com/zeebe-io/zeebe/clients/go/worker"
	"github.com/zeebe-io/zeebe/clients/go/zbc"
)

/*
	what does this code need to do?
		- subscribe to a particular zeebe task type
		- call the nomad API to create a new nomad job passing a unique id for the sub transaction and any other relevant parameters

	dependencies
		external
			- zeebe broker [x]
			- nomad cluster [x]
			- nomad job [x]
			- nomad job definition [x] - need to do some research on 'batch' jobs
			- nomad job docker image [x]

		internal
			- the URL of the zeebe broker [x]
			- the zeebe 'job type'(s) [x]
			- the URL of a nomad server [x]
			- a JSON representation of the nomad job to run []
			- a unique id for the nomad job [x] - zeebe 'job key'

	notes
		like nomad jobs of type 'service', nomad jobs of type 'batch' are identified by a unique id and
		if the nomad server cluster receives any subsequent 'create job' commands/requests then it updates the
		job's state according to the incoming parameters but DOES NOT start a new instance of the job.
*/

func main() {
	zeebeBrokerURL := mustGetEnv("ZEEBE_BROKER_URL")
	nomadServerURL := mustGetEnv("NOMAD_SERVER_URL")

	zeebeTasksToFail := map[string]struct{}{}
	for _, task := range strings.Split(os.Getenv("ZEEBE_TASKS_TO_FAIL"), ",") {
		zeebeTasksToFail[task] = struct{}{}
	}

	cfg := nomadAPI.Config{
		Address: nomadServerURL,
	}
	nomadClient, err := nomadAPI.NewClient(&cfg)
	if err != nil {
		log.Fatalf("error creating nomad client: %v", err)
	}

	zeebeClient, err := zbc.NewZBClient(zeebeBrokerURL)
	if err != nil {
		log.Fatalf("error creating zeebe client: %#v", err)
	}

	nomadJobJSON, err := os.Open(mustGetEnv("NOMAD_JOB_JSON_PATH"))
	if err != nil {
		log.Fatalf("error opening JSON file: %v", err)
	}

	var job nomadAPI.Job
	if err := json.NewDecoder(nomadJobJSON).Decode(&job); err != nil {
		log.Fatalf("error decoding nomad job JSON: %v", err)
	}

	type JobType struct {
		name    string
		handler func(worker.JobClient, entities.Job)
	}

	jobTypes := []string{
		"action",
		"test",
		"rollback",
	}

	var wg sync.WaitGroup
	for _, jt := range jobTypes {
		wg.Add(1)
		go func(jt string) {
			defer wg.Done()

			log.Println("Starting job worker for type:", jt)
			handler := nomadTriggerHandler(jt, "10.0.2.2:26500", zeebeTasksToFail, nomadClient, newNomadJobFactory(job))
			jobWorker := zeebeClient.NewJobWorker().JobType(jt).Handler(handler).Timeout(time.Second * 2).Open()
			defer jobWorker.Close()
			jobWorker.AwaitClose()
		}(jt)
	}

	wg.Wait()
}

type NomadJobFactory func() nomadAPI.Job

func newNomadJobFactory(nomadJob nomadAPI.Job) NomadJobFactory {
	return func() nomadAPI.Job {
		return nomadJob
	}
}

func nomadTriggerHandler(zeebeJobType, zeebeBrokerURL string, zeebeTasksToFail map[string]struct{}, nomadClient *nomadAPI.Client, nomadJobFactory NomadJobFactory) func(worker.JobClient, entities.Job) {
	return func(zeebeClient worker.JobClient, zeebeJob entities.Job) {
		transactionID := fmt.Sprintf(
			"%s_%d_%s_%d",
			zeebeJob.JobHeaders.BpmnProcessId,
			zeebeJob.JobHeaders.WorkflowInstanceKey,
			zeebeJob.JobHeaders.ElementId,
			zeebeJob.Key,
		)

		zeebePayload, err := zeebeJob.GetPayloadAsMap()
		if err != nil {
			failZeebeJob(zeebeClient, zeebeJob, err)
			return
		}
		zeebePayload["error"] = false

		zeebePayloadJSON, err := json.Marshal(zeebePayload)
		if err != nil {
			failZeebeJob(zeebeClient, zeebeJob, err)
			return
		}

		log.Println(zeebeJob.GetPayload())
		log.Println(string(zeebePayloadJSON))

		nomadJob := nomadJobFactory()
		nomadJob.ID = &transactionID
		nomadJob.Name = &transactionID
		nomadJob.TaskGroups[0].Tasks[0].Env["ZEEBE_JOB_KEY"] = strconv.Itoa(int(zeebeJob.Key))
		nomadJob.TaskGroups[0].Tasks[0].Env["ZEEBE_PAYLOAD"] = string(zeebePayloadJSON)
		nomadJob.TaskGroups[0].Tasks[0].Env["ZEEBE_BROKER_URL"] = zeebeBrokerURL
		nomadJob.TaskGroups[0].Tasks[0].Env["ZEEBE_FAIL_JOB_FLAG"] = "false"
		nomadJob.TaskGroups[0].Tasks[0].Env["DURATION"] = "2m"

		if _, ok := zeebeTasksToFail[zeebeJob.JobHeaders.ElementId]; ok {
			nomadJob.TaskGroups[0].Tasks[0].Env["ZEEBE_FAIL_JOB_FLAG"] = "true"
		}

		// check for job existence
		nomadJobExists := true
		log.Printf("querying for nomad job with id %s\n", transactionID)
		nomadJobResult, _, err := nomadClient.Jobs().Info(transactionID, nil)
		if err != nil {
			if strings.Contains(err.Error(), "404") { // dirty hack!!!
				nomadJobExists = false
			} else {
				log.Fatalf("error querying nomad job: %v", err)
			}
		}

		if nomadJobExists {
			log.Printf("job %s exists. Status: %s\n", transactionID, *nomadJobResult.Status)
			return
		}

		// create job
		log.Printf("creating nomad job with id %s\n", transactionID)
		if _, _, err = nomadClient.Jobs().Register(&nomadJob, nil); err != nil {
			log.Fatalf("error creating nomad job: %v", err)
		}
	}
}

/*
	Given zeebe's limitations around error handling. How can we gain error transitioning in it's
	current state?

	Constraints
	 - if a job is failed 'the zeebe way' i.e. using the NewFailJobCommand method then we can retry but can't transition.
	 - if we flag a job as failed in the payload then we can transition using the JSONPath switch
*/
func failZeebeJob(client worker.JobClient, job entities.Job, err error) {
	if err != nil {
		log.Printf("error handling action: %v\n", err)
	}

	payload, err := job.GetPayloadAsMap()
	if err != nil {
		log.Fatalf("error getting payload: %v", err)
		return
	}
	payload["error"] = true

	req, err := client.NewCompleteJobCommand().JobKey(job.Key).PayloadFromMap(payload)
	if err != nil {
		log.Fatalf("error completing job: %v", err)
		return
	}

	if _, err := req.Send(); err != nil {
		log.Fatalf("error sending request: %v", err)
		return
	}

	log.Println("Failed job", job.JobHeaders.ElementId, "of type", job.Type)
}

func mustGetEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env var '%s' not set", k)
	}
	return v
}
